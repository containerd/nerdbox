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

package task

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

// uniquePipePath returns a process-unique named-pipe path for a test.
func uniquePipePath(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\nerdbox-test-%d`, time.Now().UnixNano())
}

// startPipeServer calls ListenPipe and, on success, spawns a goroutine that
// accepts and immediately closes a single connection. It touches no *testing.T,
// so it is safe to call from a goroutine; the listener (or the ListenPipe
// error) is returned to the caller, which owns assertions and cleanup.
func startPipeServer(path string) (net.Listener, error) {
	l, err := winio.ListenPipe(path, nil)
	if err != nil {
		return nil, err
	}
	go func() {
		if c, aerr := l.Accept(); aerr == nil {
			c.Close()
		}
	}()
	return l, nil
}

// TestDialPipeWaitingServerPresent: an existing pipe server connects on the
// first dial, exactly like winio.DialPipe.
func TestDialPipeWaitingServerPresent(t *testing.T) {
	path := uniquePipePath(t)
	l, err := startPipeServer(path)
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	conn, err := dialPipeWaiting(t.Context(), path)
	if err != nil {
		t.Fatalf("dial with server present: %v", err)
	}
	conn.Close()
}

// TestDialPipeWaitingServerArrivesLate: dialing must poll past
// ERROR_FILE_NOT_FOUND and connect once the server calls ListenPipe.
func TestDialPipeWaitingServerArrivesLate(t *testing.T) {
	path := uniquePipePath(t)

	type serverResult struct {
		l   net.Listener
		err error
	}
	// Buffered so the goroutine never blocks on send, and never touches t.
	ready := make(chan serverResult, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		l, err := startPipeServer(path)
		ready <- serverResult{l: l, err: err}
	}()

	start := time.Now()
	conn, err := dialPipeWaiting(t.Context(), path)
	if err != nil {
		t.Fatalf("dial with late server: %v", err)
	}
	conn.Close()

	res := <-ready
	if res.err != nil {
		t.Fatalf("ListenPipe: %v", res.err)
	}
	t.Cleanup(func() { res.l.Close() })

	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("dial returned in %s; expected it to wait for the server", elapsed)
	}
}

// TestDialPipeWaitingNoServerTimesOut: with no server, dialing fails fast once
// the context deadline elapses rather than blocking forever.
func TestDialPipeWaitingNoServerTimesOut(t *testing.T) {
	path := uniquePipePath(t)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	conn, err := dialPipeWaiting(ctx, path)
	if err == nil {
		conn.Close()
		t.Fatal("expected an error with no server present, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("dial took %s to give up; expected it to honor the context deadline", elapsed)
	}
}
