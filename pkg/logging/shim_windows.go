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

package logging

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// openShimLog creates a named pipe for containerd to read shim logs and
// redirects os.Stderr to it, mirroring the containerd shim framework's
// Windows openLog behavior.
//
// The namespace and id are read from os.Args by the caller
// ([SetupShimLog]) and passed to this function as the ns and id
// parameters.
func openShimLog(ns, id string) io.Writer {
	if ns == "" || id == "" {
		return os.Stderr
	}
	pipeName := fmt.Sprintf(`\\.\pipe\containerd-shim-%s-%s-log`, ns, id)
	l, err := winio.ListenPipe(pipeName, nil)
	if err != nil {
		return os.Stderr
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		l.Close()
		return os.Stderr
	}

	oldStderr := os.Stderr
	os.Stderr = pw
	windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(pw.Fd())) //nolint:errcheck

	go serveShimLog(l, pr, oldStderr)

	return pw
}

// serveShimLog copies the shim's stderr from r to stderr and to the most
// recently accepted connection on l.
//
// containerd connects when it starts the shim and again when a restarted
// containerd reloads it, so l stays open for the lifetime of the shim. A
// failed write drops the connection but r keeps being drained: if the copy
// stopped when containerd went away, the pipe buffer would fill and every
// later stderr write in the shim, including logging in RPC handlers, would
// block.
//
// A reader that is connected but not reading still blocks the copy, as a
// FIFO with a live reader does on Unix, until a new connection replaces it.
// Writes happen outside mu so that a new connection can always be accepted,
// and closing the old one unblocks the pending write.
func serveShimLog(l net.Listener, r io.Reader, stderr io.Writer) {
	var (
		mu     sync.Mutex
		conn   net.Conn
		closed bool
	)

	defer func() {
		l.Close()

		mu.Lock()
		closed = true
		if conn != nil {
			conn.Close()
			conn = nil
		}
		mu.Unlock()
	}()

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			if closed {
				mu.Unlock()
				c.Close()
				return
			}
			old := conn
			conn = c
			mu.Unlock()

			if old != nil {
				old.Close()
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			stderr.Write(buf[:n]) //nolint:errcheck

			mu.Lock()
			c := conn
			mu.Unlock()

			if c != nil {
				if _, werr := c.Write(buf[:n]); werr != nil {
					c.Close()

					mu.Lock()
					if conn == c {
						conn = nil
					}
					mu.Unlock()
				}
			}
		}
		if err != nil {
			return
		}
	}
}
