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

package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// testPipeAddr returns a unique named pipe address for a test.
func testPipeAddr(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\nerdbox-test-%d`, time.Now().UnixNano())
}

// startPipeServer launches a goroutine that, after listenDelay, calls
// ListenPipe and then (after acceptDelay) Accept. Errors are reported on a
// channel that t.Cleanup drains so the goroutine never calls t.* methods.
// Cleanup also actively closes the listener so a blocked Accept unblocks
// and the goroutine cannot outlive the test even on t.Fatal.
func startPipeServer(t *testing.T, addr string, listenDelay, acceptDelay time.Duration) {
	t.Helper()

	listenerCh := make(chan net.Listener, 1)
	done := make(chan error, 1)

	go func() {
		defer close(done)
		if listenDelay > 0 {
			time.Sleep(listenDelay)
		}
		l, err := winio.ListenPipe(addr, nil)
		if err != nil {
			done <- fmt.Errorf("ListenPipe: %w", err)
			listenerCh <- nil
			return
		}
		listenerCh <- l
		if acceptDelay > 0 {
			time.Sleep(acceptDelay)
		}
		conn, err := l.Accept()
		if err == nil {
			conn.Close()
		}
		// Accept errors are not reported: when the test ends, cleanup closes
		// the listener which makes Accept return an error that is uninteresting
		// to assert on. The listener is closed by cleanup, not here.
	}()

	t.Cleanup(func() {
		// Wait for ListenPipe to complete so we can close the listener and
		// unblock any in-progress Accept.
		select {
		case l := <-listenerCh:
			if l != nil {
				l.Close()
			}
		case <-time.After(2 * time.Second):
			t.Errorf("pipe server: ListenPipe did not complete within 2s")
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("pipe server goroutine: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("pipe server goroutine did not exit within 2s")
		}
	})
}

// TestWaitForShimPipe_SlowListen verifies success when the server starts
// listening after a short delay but before the overall deadline.
func TestWaitForShimPipe_SlowListen(t *testing.T) {
	addr := testPipeAddr(t)
	startPipeServer(t, addr, 300*time.Millisecond, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var shimExit <-chan error // never signals shim exit in this test

	if err := waitForShimPipe(ctx, addr, shimExit, 5*time.Second, 50*time.Millisecond, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForShimPipe returned unexpected error: %v", err)
	}
}

// TestWaitForShimPipe_SlowAccept verifies that waitForShimPipe retries
// instead of returning early when DialPipe returns ErrTimeout because the
// server has called ListenPipe but has not yet reached Accept(). This is the
// primary bug fixed by this change.
func TestWaitForShimPipe_SlowAccept(t *testing.T) {
	addr := testPipeAddr(t)
	startPipeServer(t, addr, 0, 600*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var shimExit <-chan error // never signals shim exit in this test

	if err := waitForShimPipe(ctx, addr, shimExit, 5*time.Second, 50*time.Millisecond, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForShimPipe returned unexpected error (early-exit regression): %v", err)
	}
}

// TestWaitForShimPipe_Timeout verifies that waitForShimPipe returns a
// descriptive error mentioning the pipe address when no server appears.
func TestWaitForShimPipe_Timeout(t *testing.T) {
	addr := testPipeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var shimExit <-chan error // never signals shim exit in this test

	err := waitForShimPipe(ctx, addr, shimExit, 200*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error %q does not mention pipe address %q", err, addr)
	}
}

// TestWaitForShimPipe_ShimExit verifies that waitForShimPipe returns
// promptly when the shim signals exit on the channel, instead of continuing
// to poll until readyTimeout elapses. Covers both a non-zero (crash) exit
// and a clean exit, since each takes a different formatting branch.
func TestWaitForShimPipe_ShimExit(t *testing.T) {
	tests := []struct {
		name       string
		exitErr    error
		wantErrSub string // substring required in the returned error message
	}{
		{
			name:       "crash",
			exitErr:    errors.New("simulated shim crash"),
			wantErrSub: "simulated shim crash",
		},
		{
			name:       "clean exit",
			exitErr:    nil,
			wantErrSub: "exit code 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// No server — DialPipe will fail with ErrNotExist on every
			// iteration so the select is always reached and observes the
			// pre-buffered shim exit signal.
			addr := testPipeAddr(t)

			shimExit := make(chan error, 1)
			shimExit <- tc.exitErr

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			start := time.Now()
			err := waitForShimPipe(ctx, addr, shimExit,
				5*time.Second, 50*time.Millisecond, 10*time.Millisecond)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "shim exited before creating pipe") {
				t.Errorf("error %q does not mention shim exit", err)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("error %q does not contain %q", err, tc.wantErrSub)
			}
			if tc.exitErr != nil && !errors.Is(err, tc.exitErr) {
				t.Errorf("error %v does not wrap %v", err, tc.exitErr)
			}
			// readyTimeout is 5s; the helper must return well before then.
			if elapsed >= time.Second {
				t.Errorf("waitForShimPipe took %s after shim exit signal; expected prompt return", elapsed)
			}
		})
	}
}

// TestWaitForShimPipe_CtxCancelled verifies that a pre-cancelled caller
// context surfaces context.Canceled rather than a timeout message and that
// the helper returns promptly without attempting a DialPipe.
func TestWaitForShimPipe_CtxCancelled(t *testing.T) {
	addr := testPipeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var shimExit <-chan error // never signals shim exit in this test

	start := time.Now()
	err := waitForShimPipe(ctx, addr, shimExit, 5*time.Second, 200*time.Millisecond, 10*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error %q does not surface context.Canceled", err)
	}
	// A pre-cancelled context should short-circuit before DialPipe, so the
	// total elapsed time must be well under one perAttempt (200ms).
	if elapsed >= 100*time.Millisecond {
		t.Errorf("waitForShimPipe took %s on pre-cancelled context; expected short-circuit", elapsed)
	}
}
