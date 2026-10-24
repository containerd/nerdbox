//go:build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package streaming

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestClosedVsockStreamRemainsClaimable(t *testing.T) {
	l, err := vsock.ListenContextID(vsock.Local, 0, nil)
	skipUnavailableVsock(t, err)
	require.NoError(t, err)
	defer l.Close()
	s := &service{l: l, streams: make(map[string]*registration), pending: make(map[net.Conn]struct{})}
	go s.Run()
	defer s.Shutdown(t.Context())
	host, err := vsock.Dial(vsock.Local, l.Addr().(*vsock.Addr).Port, nil)
	skipUnavailableVsock(t, err)
	require.NoError(t, err)
	defer host.Close()
	require.NoError(t, host.SetDeadline(time.Now().Add(5*time.Second)))
	const id = "delayed-socket-forward"
	require.NoError(t, writeString(host, id))
	var n uint32
	require.NoError(t, binary.Read(host, binary.BigEndian, &n))
	ack := make([]byte, n)
	_, err = io.ReadFull(host, ack)
	require.NoError(t, err)
	require.Equal(t, id, string(ack))
	const payload = "greeting before close"
	_, err = io.WriteString(host, payload)
	require.NoError(t, err)
	require.NoError(t, host.Close())
	// A socket-forward relay may finish before its ConnectResult allows
	// the guest to claim the stream. Peer closure does not cancel ownership.
	time.Sleep(1100 * time.Millisecond)
	conn, err := s.Get(id)
	require.NoError(t, err)
	defer conn.Close()
	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	require.Equal(t, payload, string(got))
	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

func skipUnavailableVsock(t *testing.T, err error) {
	t.Helper()
	for _, unavailable := range []error{unix.EAFNOSUPPORT, unix.EPROTONOSUPPORT, unix.ENODEV, unix.ENETUNREACH, unix.EPERM} {
		if errors.Is(err, unavailable) {
			t.Skipf("AF_VSOCK loopback unavailable: %v", err)
		}
	}
}
