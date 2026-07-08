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
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
)

// recordingStreamConn records everything written to it and whether CloseWrite
// has been called, so a test can assert that all data was forwarded before EOF
// was signalled to the guest.
type recordingStreamConn struct {
	mu               sync.Mutex
	buf              bytes.Buffer
	closeWriteCalled bool
	bytesAfterClose  int
}

func (c *recordingStreamConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeWriteCalled {
		c.bytesAfterClose += len(p)
	}
	return c.buf.Write(p)
}

func (c *recordingStreamConn) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeWriteCalled = true
	return nil
}

// gatedChunkReader returns pre-set chunks one per Read, delivering EOF after
// the last chunk. The first Read signals on started and then blocks until gate
// is closed.
type gatedChunkReader struct {
	chunks  [][]byte
	idx     int
	started chan struct{}
	gate    <-chan struct{}
	gated   bool
}

func (r *gatedChunkReader) Read(p []byte) (int, error) {
	if !r.gated {
		close(r.started)
		<-r.gate
		r.gated = true
	}
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

// TestCopyStdinUntilCloseDrainsOnCloseIO is a regression test for stdin
// truncation on CloseIO. When CloseIO fires while the stdin reader has fallen
// behind (more than one buffer's worth of data still buffered), the closeCh
// branch must drain all remaining data before calling CloseWrite.
func TestCopyStdinUntilCloseDrainsOnCloseIO(t *testing.T) {
	chunks := [][]byte{
		[]byte("AAAA"),
		[]byte("BBBB"),
		[]byte("CCCC"),
	}
	const want = "AAAABBBBCCCC"

	gate := make(chan struct{})
	closeCh := make(chan context.Context, 1)
	sc := &recordingStreamConn{}
	// Buffer larger than a single chunk so each Read returns exactly one
	// chunk (mirroring one FIFO read per iteration).
	buf := make([]byte, 8)
	r := &gatedChunkReader{chunks: chunks, started: make(chan struct{}), gate: gate}

	done := make(chan struct{})
	go func() {
		copyStdinUntilClose(context.Background(), sc, r, buf, closeCh)
		close(done)
	}()

	// Wait until the first read has begun and parked on gate. From here the
	// read channel cannot become ready until we close gate, so firing CloseIO
	// now forces the select down the closeCh branch deterministically. The
	// CloseIO context is never cancelled, so the drain runs through to EOF.
	<-r.started
	closeCh <- context.Background()

	// Release the reader so the closeCh branch can drain every remaining chunk.
	close(gate)

	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("copyStdinUntilClose did not return")
	}

	if got := sc.buf.String(); got != want {
		t.Fatalf("stdin truncated on CloseIO: got %q (%d bytes), want %q (%d bytes)",
			got, len(got), want, len(want))
	}
	if !sc.closeWriteCalled {
		t.Fatal("CloseWrite was not called")
	}
	if sc.bytesAfterClose != 0 {
		t.Fatalf("%d bytes written after CloseWrite; EOF was signalled before draining",
			sc.bytesAfterClose)
	}
}

// blockingReader parks on every Read until release, then reports EOF. It models
// a FIFO whose write end is never closed, so a read can never complete on its
// own.
type blockingReader struct {
	release <-chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// TestStdinEOFFuncDeliversEOFWhenContextAlreadyCancelled is a regression test
// for CloseIO leaving stdin open. When CloseIO is invoked with a context that is
// already cancelled, stdinEOFFunc must still signal the copy goroutine so it
// sends the in-band EOF (CloseWrite); it must not short-circuit on the cancelled
// context and return with the request undelivered.
func TestStdinEOFFuncDeliversEOFWhenContextAlreadyCancelled(t *testing.T) {
	closeCh := make(chan context.Context, 1)
	drainDone := make(chan struct{})
	release := make(chan struct{})
	sc := &recordingStreamConn{}
	buf := make([]byte, 8)
	r := &blockingReader{release: release}

	go func() {
		copyStdinUntilClose(context.Background(), sc, r, buf, closeCh)
		close(drainDone)
	}()

	stdinEOF := stdinEOFFunc(closeCh, drainDone)

	// CloseIO fires with an already-cancelled context. The drain cannot read to
	// EOF (the reader is parked), so delivery of the in-band EOF depends solely
	// on the request reaching the copy goroutine.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stdinEOF(cctx); err != nil && err != context.Canceled {
		t.Fatalf("stdinEOF returned unexpected error: %v", err)
	}

	select {
	case <-drainDone:
	case <-t.Context().Done():
		t.Fatal("copy goroutine was not signalled; in-band EOF never delivered")
	}

	// Let the parked read return so its goroutine does not leak.
	close(release)

	if !sc.closeWriteCalled {
		t.Fatal("CloseWrite was not called; stdin left open despite CloseIO")
	}
}

// stallingReader delivers its chunks one per Read and then blocks forever,
// modelling a FIFO whose write end is never closed (so a read can never see
// EOF). blocking is closed when the reader parks; release lets the parked read
// finally return so the test leaks no goroutine.
type stallingReader struct {
	chunks   [][]byte
	idx      int
	blocking chan struct{}
	release  <-chan struct{}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		// No buffered data left and the writer never closed: block like a
		// real FIFO read waiting on data or an EOF that never arrives.
		close(r.blocking)
		<-r.release
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

// TestCopyStdinUntilCloseAbortsDrainOnContextCancel is a regression test for
// the stdin drain wedging when CloseIO is issued but the FIFO write end is
// never closed. Because the drain is bound to the CloseIO request context,
// cancelling that context (the caller giving up) must forward all buffered
// data, then unblock the drain and still deliver the in-band EOF.
func TestCopyStdinUntilCloseAbortsDrainOnContextCancel(t *testing.T) {
	chunks := [][]byte{
		[]byte("AAAA"),
		[]byte("BBBB"),
	}
	const want = "AAAABBBB"

	closeCh := make(chan context.Context, 1)
	release := make(chan struct{})
	sc := &recordingStreamConn{}
	buf := make([]byte, 8)
	r := &stallingReader{chunks: chunks, blocking: make(chan struct{}), release: release}

	// cctx models the CloseIO request context; cancelling it stands in for the
	// caller cancelling or hitting its deadline.
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		copyStdinUntilClose(context.Background(), sc, r, buf, closeCh)
		close(done)
	}()

	// CloseIO fires, but the writer is never closed: the drain forwards the
	// buffered chunks and then parks on a read that can never complete.
	closeCh <- cctx

	// Once the drain is parked on the never-completing read, cancelling the
	// CloseIO context must unblock it deterministically (no timing dependence).
	<-r.blocking
	cancel()

	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("copyStdinUntilClose did not return after context cancel")
	}

	// Let the abandoned background read return so no goroutine is leaked.
	close(release)

	if got := sc.buf.String(); got != want {
		t.Fatalf("stdin data lost before cancel abort: got %q (%d bytes), want %q (%d bytes)",
			got, len(got), want, len(want))
	}
	if !sc.closeWriteCalled {
		t.Fatal("CloseWrite was not called on context cancel")
	}
	if sc.bytesAfterClose != 0 {
		t.Fatalf("%d bytes written after CloseWrite; EOF was signalled before draining",
			sc.bytesAfterClose)
	}
}
