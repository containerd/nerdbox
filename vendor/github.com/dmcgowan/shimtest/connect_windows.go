//go:build windows

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

// Windows connection layer for shimtest.
//
// On Windows the shim communicates over Windows named pipes rather than Unix
// domain sockets. This file provides Windows-specific implementations of the
// three connection helpers:
//
//   connectShim     — dials the shim's TTRPC named pipe
//   containerdSockPath — returns the named pipe path the shim dials for events
//   listenEvents    — creates the named pipe server for the events endpoint

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// connectShim dials the shim's TTRPC named pipe. The address returned by the
// shim's "start" subcommand is a Windows named pipe path
// (\\.\pipe\containerd-shim-<id>). We use winio.AnonDialer semantics:
// retry for up to 5 s to give the shim time to start listening.
func connectShim(tb testing.TB, address string) net.Conn {
	tb.Helper()
	addr := strings.TrimPrefix(address, "ttrpc+npipe://")
	addr = strings.TrimPrefix(addr, "npipe://")
	// address is typically already the raw pipe path, e.g.
	// \\.\pipe\containerd-shim-<hex>
	conn, err := dialNamedPipeRetry(addr, 10*time.Second)
	if err != nil {
		tb.Fatalf("failed to connect to shim at %s: %v", addr, err)
	}
	tb.Cleanup(func() { conn.Close() })
	return conn
}

// dialShimConn dials the shim TTRPC address for use in shimPidViaConnect.
func dialShimConn(address string, timeout time.Duration) (net.Conn, error) {
	addr := strings.TrimPrefix(address, "ttrpc+npipe://")
	addr = strings.TrimPrefix(addr, "npipe://")
	return dialNamedPipeRetry(addr, timeout)
}

// dialNamedPipeRetry dials a Windows named pipe, retrying until the pipe
// exists or timeout is reached. Mirrors containerd's AnonDialer in
// pkg/shim/util_windows.go.
func dialNamedPipeRetry(path string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	serveTimer := time.NewTimer(5 * time.Second)
	defer serveTimer.Stop()
	for {
		c, err := winio.DialPipeContext(ctx, path)
		if err == nil {
			return c, nil
		}
		// Pipe not yet created — keep retrying until the 5 s serve timer fires.
		select {
		case <-serveTimer.C:
			return nil, fmt.Errorf("pipe not found before timeout: %w", err)
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for pipe %s: %w", path, ctx.Err())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// containerdSockPath returns the Windows named pipe path that the shim dials
// as its containerd events endpoint. The path is unique per bundleDir and is
// cached so all callers within the same test receive the same path.
func containerdSockPath(tb testing.TB, bundleDir string) string {
	tb.Helper()
	if v, ok := shortSocketPaths.Load(bundleDir); ok {
		return v.(string)
	}
	p := `\\.\pipe\shimtest-events-` + randomSuffix()
	shortSocketPaths.Store(bundleDir, p)
	tb.Cleanup(func() { shortSocketPaths.Delete(bundleDir) })
	return p
}

// listenEvents creates a Windows named-pipe server at socketPath for the TTRPC
// events endpoint. The shim connects to this pipe to publish container lifecycle
// events. Mirrors how containerd exposes its events endpoint on Windows.
func listenEvents(tb testing.TB, socketPath string) net.Listener {
	tb.Helper()
	ln, err := winio.ListenPipe(socketPath, pipeCfg)
	if err != nil {
		tb.Fatalf("events ListenPipe %s: %v", socketPath, err)
	}
	return ln
}
