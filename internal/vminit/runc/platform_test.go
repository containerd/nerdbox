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

package runc

import (
	"bytes"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/containerd/console"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type consoleStream struct {
	io.ReadWriteCloser
	closed      bool
	writeClosed bool
	data        bytes.Buffer
}

func (*consoleStream) Read([]byte) (int, error)      { return 0, io.EOF }
func (s *consoleStream) Write(p []byte) (int, error) { return s.data.Write(p) }
func (s *consoleStream) CloseWrite() error           { s.writeClosed = true; return nil }
func (s *consoleStream) Close() error                { s.closed = true; return nil }

type consoleStreams map[string]*consoleStream

func (m consoleStreams) Get(id string) (io.ReadWriteCloser, error) { return m[id], nil }

func TestConsoleOwnsStdoutStream(t *testing.T) {
	stdout := &consoleStream{}
	p, err := NewPlatform(consoleStreams{"out": stdout})
	require.NoError(t, err)
	defer p.Close()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	// Received runc console descriptors bypass Go's runtime poller.
	master, slavePath, err := console.NewPtyFromFile(os.NewFile(uintptr(fd), "/dev/ptmx"))
	require.NoError(t, err)
	defer master.Close()
	var wg sync.WaitGroup
	cons, err := p.CopyConsole(t.Context(), master, "tty", "", "stream://out", "", &wg)
	require.NoError(t, err)
	slave, err := os.OpenFile(slavePath, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = slave.Write([]byte("output before exit"))
	require.NoError(t, err)
	require.NoError(t, slave.Close())
	drained := make(chan error, 1)
	go func() {
		err := p.ShutdownConsole(t.Context(), cons)
		wg.Wait()
		drained <- err
	}()
	select {
	case err := <-drained:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("console output did not drain after shutdown")
	}
	require.False(t, stdout.closed, "output drain must not close the transport before delete")
	require.True(t, stdout.writeClosed)
	require.Equal(t, "output before exit", stdout.data.String())
	require.NoError(t, cons.Close())
	require.True(t, stdout.closed, "console stdout stream is not owned by processIO")
}
