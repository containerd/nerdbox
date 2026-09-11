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

package libkrun

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// TestStartStreamUnblockedByShutdown verifies that a StartStream call
// blocked in the guest handshake (waiting for an ack that will never come)
// returns with an error as soon as its connection is closed the way
// Shutdown's closeStreamConnsLocked does, instead of hanging forever.
func TestStartStreamUnblockedByShutdown(t *testing.T) {
	guestConn, hostConn := net.Pipe()
	defer guestConn.Close()

	// Simulate a guest that reads the stream ID but never acks it.
	received := make(chan struct{})
	go func() {
		var idLen uint32
		if err := binary.Read(guestConn, binary.BigEndian, &idLen); err != nil {
			return
		}
		io.ReadFull(guestConn, make([]byte, idLen))
		close(received)
		// Deliberately never writes an ack back.
	}()

	v := &vmInstance{inFlightHandshakes: make(map[net.Conn]struct{})}
	if !v.trackStreamConn(hostConn) {
		t.Fatal("trackStreamConn should succeed before Shutdown has run")
	}

	handshakeDone := make(chan error, 1)
	go func() {
		handshakeDone <- completeStreamHandshake(hostConn, "test-stream")
	}()

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("guest side never received the stream id")
	}

	// completeStreamHandshake must now be blocked reading the ack that
	// will never come. Simulate Shutdown closing every tracked stream
	// connection.
	v.mu.Lock()
	v.closeStreamConnsLocked()
	v.mu.Unlock()

	select {
	case err := <-handshakeDone:
		if err == nil {
			t.Fatal("expected an error once the connection was closed out from under the handshake")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not return after its connection was closed")
	}
}

// TestStartStreamFailsFastOnceShutdown verifies that StartStream, called
// after Shutdown has already cleared inFlightHandshakes, fails fast instead of
// completing a handshake against a VM that is gone.
func TestStartStreamFailsFastOnceShutdown(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("failed to chdir to temp dir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(origWD) })

	const streamPath = "streaming.sock"
	l, err := net.Listen("unix", streamPath)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", streamPath, err)
	}
	defer l.Close()

	// Accept connections but never respond: if StartStream reached the
	// handshake, it would hang. It must not get that far. Connections are
	// collected and closed once the loop exits (listener closed) rather
	// than via a defer inside the loop, which would stack one deferred
	// close per accepted connection until the goroutine returns anyway.
	go func() {
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conns = append(conns, conn)
		}
	}()

	// inFlightHandshakes is left nil, matching the post-Shutdown state.
	v := &vmInstance{streamPath: streamPath}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := v.StartStream(ctx, "test-stream"); err == nil {
		t.Fatal("expected StartStream to fail fast once inFlightHandshakes is nil")
	}
}
