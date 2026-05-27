//go:build !windows

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

package shimtest

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// connectShim dials the shim's TTRPC unix socket.
func connectShim(tb testing.TB, address string) net.Conn {
	tb.Helper()
	addr := strings.TrimPrefix(address, "unix://")
	conn, err := net.Dial("unix", addr)
	if err != nil {
		tb.Fatalf("failed to connect to shim at %s: %v", addr, err)
	}
	tb.Cleanup(func() { conn.Close() })
	return conn
}

// dialShimConn dials the shim address with retries for use in
// shimPidViaConnect. Returns the connection or an error.
func dialShimConn(address string, _ time.Duration) (net.Conn, error) {
	addr := strings.TrimPrefix(address, "unix://")
	return net.Dial("unix", addr)
}

// containerdSockPath returns the unix socket path the shim dials as
// its containerd events endpoint. On macOS, AF_UNIX paths are limited
// to 104 bytes, so a short /tmp path is used when the bundle is too
// deep. The result is cached per bundleDir so all callers within a
// single test get the same path.
func containerdSockPath(tb testing.TB, bundleDir string) string {
	tb.Helper()
	candidate := filepath.Join(bundleDir, "c.sock")
	if len(candidate) <= 104 {
		return candidate
	}
	if v, ok := shortSocketPaths.Load(bundleDir); ok {
		return v.(string)
	}
	dir, err := os.MkdirTemp("", "nb-ev-")
	if err != nil {
		tb.Fatal("create short socket dir:", err)
	}
	p := filepath.Join(dir, "c.sock")
	shortSocketPaths.Store(bundleDir, p)
	tb.Cleanup(func() {
		shortSocketPaths.Delete(bundleDir)
		os.RemoveAll(dir)
	})
	return p
}

// listenEvents creates a Unix-domain socket listener at socketPath for
// use by the TTRPC events server.
func listenEvents(tb testing.TB, socketPath string) net.Listener {
	tb.Helper()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		tb.Fatal("events listen:", err)
	}
	return ln
}
