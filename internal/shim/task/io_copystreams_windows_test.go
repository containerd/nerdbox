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
	"fmt"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
	"github.com/stretchr/testify/require"
)

// fakeStdinStream is a minimal in-memory stdinStreamWriteCloser used to
// observe what copyStreams' stdin goroutine relays to the "guest" side
// (Write) and whether/when it delivers EOF (CloseWrite), without needing a
// real vsock connection or VM. It never returns anything to Read: the
// stdin direction only writes into it.
type fakeStdinStream struct {
	mu          sync.Mutex
	buf         []byte
	writeClosed bool
}

func newFakeStdinStream() *fakeStdinStream {
	return &fakeStdinStream{}
}

func (f *fakeStdinStream) Read([]byte) (int, error) { return 0, io.EOF }

func (f *fakeStdinStream) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf = append(f.buf, p...)
	return len(p), nil
}

func (f *fakeStdinStream) Close() error { return nil }

func (f *fakeStdinStream) CloseWrite() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeClosed = true
	return nil
}

// snapshot returns the bytes relayed so far and whether CloseWrite (EOF)
// has been delivered.
func (f *fakeStdinStream) snapshot() (data string, eofDelivered bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.buf), f.writeClosed
}

// testPipeName returns a unique named-pipe path for this test process.
func testPipeName(t *testing.T) string {
	t.Helper()
	return `\\.\pipe\nerdbox-copystreams-test-` + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// TestCopyStreamsStdinDetachReattach exercises the Windows stdin redial
// logic in copyStreams (io_copystreams_windows.go) directly, without any
// VM/libkrun/shim process involved. It validates the core contract added
// to support stdin detach/re-attach on Windows:
//
//  1. A client can write to the exec's stdin and disconnect ("detach")
//     without stdinEOF (CloseIO) having been called, and the guest-facing
//     stream must NOT see EOF (CloseWrite) during that detach window.
//  2. A new client can then connect to the very same named-pipe path
//     ("re-attach") and its data must also be relayed.
//  3. Only after stdinEOF is called does the next disconnect deliver EOF
//     (CloseWrite) to the guest-facing stream.
//
// This is the Windows analogue of the shimtest conformance test
// StdinDetachReattach (vendor/github.com/containerd/shimtest/
// exec_suite.go), but runs as a plain `go test` with no VM required, so it
// also runs automatically in CI's existing windows-latest unit-test job
// (see .github/workflows/ci.yml, task test:unit).
func TestCopyStreamsStdinDetachReattach(t *testing.T) {
	pipePath := testPipeName(t)
	l, err := winio.ListenPipe(pipePath, &winio.PipeConfig{
		InputBufferSize:  4096,
		OutputBufferSize: 4096,
	})
	require.NoError(t, err, "ListenPipe")
	defer l.Close()

	// acceptWriteClose accepts exactly one client connection, writes data
	// to it, and closes -- run in a goroutine so the caller can overlap it
	// with copyStreams' dial/redial attempts, exactly as a real client
	// would.
	acceptWriteClose := func(data string) <-chan error {
		done := make(chan error, 1)
		go func() {
			conn, err := l.Accept()
			if err != nil {
				done <- fmt.Errorf("accept: %w", err)
				return
			}
			defer conn.Close()
			if _, err := conn.Write([]byte(data)); err != nil {
				done <- fmt.Errorf("write: %w", err)
				return
			}
			done <- nil
		}()
		return done
	}

	// The first writer must already be trying to connect before
	// copyStreams dials, mirroring the ordering shimtest's exec tests use
	// (open the stdin writer before calling Exec): copyStreams' first
	// dial is synchronous and bounded by pipeDialTimeout.
	w1Done := acceptWriteClose("first")

	sc := newFakeStdinStream()
	streams := [3]io.ReadWriteCloser{sc, nil, nil}
	ioDone := make(chan struct{})

	stdinEOF, stdinDone, err := copyStreams(context.Background(), streams, pipePath, "", "", ioDone)
	require.NoError(t, err, "copyStreams")
	require.NotNil(t, stdinEOF, "expected non-nil stdinEOF")
	require.NotNil(t, stdinDone, "expected non-nil stdinDone")

	require.NoError(t, <-w1Done, "first writer")

	// Give the copy goroutine a moment to notice the first writer's
	// disconnect and start waiting to reconnect (the "detach" period).
	time.Sleep(300 * time.Millisecond)

	// Confirm no premature EOF was delivered to the guest during the
	// detach window: the shim must not deliver EOF just because a client
	// disconnected without calling CloseIO.
	if _, eof := sc.snapshot(); eof {
		t.Fatal("stdin EOF (CloseWrite) delivered during detach, before stdinEOF/CloseIO was called")
	}

	// A second writer "re-attaches" on the very same pipe path.
	w2Done := acceptWriteClose("second")
	require.NoError(t, <-w2Done, "second writer")

	// Give the copy goroutine a moment to relay the second write.
	time.Sleep(300 * time.Millisecond)

	data, eof := sc.snapshot()
	require.False(t, eof, "EOF must not be delivered before stdinEOF is called")
	require.Equal(t, "firstsecond", data, "expected data from both the pre-detach and post-reattach writers to have been relayed before stdinEOF")

	// Now signal real EOF, as CloseIO would.
	require.NoError(t, stdinEOF())

	select {
	case <-stdinDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stdinDone after stdinEOF")
	}

	data, eof = sc.snapshot()
	require.True(t, eof, "expected CloseWrite to have been called after stdinEOF")
	require.Equal(t, "firstsecond", data, "expected data from both the pre-detach and post-reattach writers")
}

// TestCopyStreamsStdinCloseWithoutDetach verifies the simple case (no
// detach involved): a single client writes and disconnects, then stdinEOF
// is called; the guest-facing stream must have received the data and EOF.
// This guards against a regression where the redial loop introduced for
// detach/re-attach support accidentally breaks the common case.
func TestCopyStreamsStdinCloseWithoutDetach(t *testing.T) {
	pipePath := testPipeName(t)
	l, err := winio.ListenPipe(pipePath, &winio.PipeConfig{
		InputBufferSize:  4096,
		OutputBufferSize: 4096,
	})
	require.NoError(t, err, "ListenPipe")
	defer l.Close()

	writeDone := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			writeDone <- fmt.Errorf("accept: %w", err)
			return
		}
		defer conn.Close()
		_, err = conn.Write([]byte("hello"))
		writeDone <- err
	}()

	sc := newFakeStdinStream()
	streams := [3]io.ReadWriteCloser{sc, nil, nil}
	ioDone := make(chan struct{})

	stdinEOF, stdinDone, err := copyStreams(context.Background(), streams, pipePath, "", "", ioDone)
	require.NoError(t, err, "copyStreams")
	require.NoError(t, <-writeDone, "writer")

	// The client already closed its connection above (defer conn.Close());
	// signal CloseIO promptly, matching the documented
	// write-then-close-then-CloseIO client protocol.
	require.NoError(t, stdinEOF())

	select {
	case <-stdinDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stdinDone")
	}

	data, eof := sc.snapshot()
	require.True(t, eof, "expected CloseWrite to have been called")
	require.Equal(t, "hello", data)
}
