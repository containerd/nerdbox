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
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type acceptResult struct {
	conn net.Conn
	err  error
}

type testListener struct {
	next   chan acceptResult
	closed chan struct{}
	once   sync.Once
}

func (l *testListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.next:
		return r.conn, r.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *testListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*testListener) Addr() net.Addr { return nil }

func testService(t *testing.T) (*service, *testListener) {
	t.Helper()
	l := &testListener{next: make(chan acceptResult, 8), closed: make(chan struct{})}
	s := &service{l: l, streams: make(map[string]*registration), pending: make(map[net.Conn]struct{})}
	go s.Run()
	t.Cleanup(func() { require.NoError(t, s.Shutdown(context.Background())) })
	return s, l
}

func registerStream(t *testing.T, l *testListener, id string) net.Conn {
	t.Helper()
	guest, host := net.Pipe()
	t.Cleanup(func() { guest.Close(); host.Close() })
	require.NoError(t, host.SetDeadline(time.Now().Add(time.Second)))
	l.next <- acceptResult{conn: guest}
	require.NoError(t, writeString(host, id))
	var n uint32
	require.NoError(t, binary.Read(host, binary.BigEndian, &n))
	b := make([]byte, n)
	_, err := io.ReadFull(host, b)
	require.NoError(t, err)
	require.Equal(t, id, string(b))
	require.NoError(t, host.SetDeadline(time.Time{}))
	return host
}

func TestIncompleteHandshakeDoesNotBlockOtherStreams(t *testing.T) {
	s, l := testService(t)
	guest, host := net.Pipe()
	defer host.Close()
	defer guest.Close()
	l.next <- acceptResult{conn: guest}
	registerStream(t, l, "healthy")
	conn, err := s.Get("healthy")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestAcceptRecoversFromFileLimit(t *testing.T) {
	s, l := testService(t)
	l.next <- acceptResult{err: &net.OpError{Op: "accept", Err: syscall.EMFILE}}
	registerStream(t, l, "after-file-limit")
	conn, err := s.Get("after-file-limit")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestShutdownClosesIncompleteHandshake(t *testing.T) {
	s, l := testService(t)
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	l.next <- acceptResult{conn: guest}
	require.NoError(t, host.SetDeadline(time.Now().Add(time.Second)))
	_, err := host.Write([]byte{0})
	require.NoError(t, err)
	require.NoError(t, s.Shutdown(t.Context()))
	_, err = host.Read(make([]byte, 1))
	require.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe), "got %v", err)
}

func TestShutdownClosesClaimedPendingACK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, l := testService(t)
		guest, host := net.Pipe()
		defer guest.Close()
		defer host.Close()
		l.next <- acceptResult{conn: guest}
		require.NoError(t, writeString(host, "pending-ack"))
		synctest.Wait()
		claimed := make(chan error, 1)
		go func() { _, err := s.Get("pending-ack"); claimed <- err }()
		synctest.Wait()
		require.NoError(t, s.Shutdown(t.Context()))
		require.Error(t, <-claimed)
	})
}

func TestAcceptsLongStreamID(t *testing.T) {
	s, l := testService(t)
	id := strings.Repeat("x", 8192)
	registerStream(t, l, id)
	conn, err := s.Get(id)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestAcceptsEmptyStreamID(t *testing.T) {
	s, l := testService(t)
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	require.NoError(t, host.SetDeadline(time.Now().Add(time.Second)))
	l.next <- acceptResult{conn: guest}
	require.NoError(t, binary.Write(host, binary.BigEndian, uint32(0)))
	var n uint32
	require.NoError(t, binary.Read(host, binary.BigEndian, &n))
	require.Zero(t, n)
	// net.Pipe synchronizes even zero-byte writes, unlike a socket.
	_, err := host.Read(nil)
	require.NoError(t, err)
	conn, err := s.Get("")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestTruncatedStreamIDIsNotRegistered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, l := testService(t)
		guest, host := net.Pipe()
		defer guest.Close()
		defer host.Close()
		l.next <- acceptResult{conn: guest}
		require.NoError(t, binary.Write(host, binary.BigEndian, uint32(20)))
		_, err := io.WriteString(host, "short")
		require.NoError(t, err)
		require.NoError(t, host.Close())
		synctest.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		require.Empty(t, s.pending)
		require.Empty(t, s.streams)
	})
}

func TestHugeStreamIDLengthWaitsForDataUntilDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, l := testService(t)
		guest, host := net.Pipe()
		defer guest.Close()
		defer host.Close()
		l.next <- acceptResult{conn: guest}
		require.NoError(t, binary.Write(host, binary.BigEndian, ^uint32(0)))
		synctest.Wait()
		s.mu.Lock()
		pending := len(s.pending)
		s.mu.Unlock()
		require.Equal(t, 1, pending)
		time.Sleep(handshakeTimeout + time.Second)
		_, err := host.Read(make([]byte, 1))
		require.ErrorIs(t, err, io.EOF)
		s.mu.Lock()
		defer s.mu.Unlock()
		require.Empty(t, s.pending)
		require.Empty(t, s.streams)
	})
}

func TestHandshakeDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, l := testService(t)
		guest, host := net.Pipe()
		defer guest.Close()
		defer host.Close()
		l.next <- acceptResult{conn: guest}
		_, err := host.Write([]byte{0})
		require.NoError(t, err)
		time.Sleep(handshakeTimeout + time.Second)
		_, err = host.Read(make([]byte, 1))
		require.ErrorIs(t, err, io.EOF)
	})
}

func TestUnclaimedStreamCanBeClaimedAfterDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, l := testService(t)
		registerStream(t, l, "prepared-exec")
		time.Sleep(24 * time.Hour)
		conn, err := s.Get("prepared-exec")
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		s.mu.Lock()
		defer s.mu.Unlock()
		require.Empty(t, s.pending)
		require.Empty(t, s.streams)
	})
}

func TestClaimedStreamDoesNotExpire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, l := testService(t)
		host := registerStream(t, l, "claimed")
		conn, err := s.Get("claimed")
		require.NoError(t, err)
		defer conn.Close()
		time.Sleep(time.Minute)
		written := make(chan error, 1)
		go func() { _, err := conn.Write([]byte("ok")); written <- err }()
		data := make([]byte, 2)
		_, err = io.ReadFull(host, data)
		require.NoError(t, err)
		require.NoError(t, <-written)
		require.Equal(t, "ok", string(data))
	})
}
