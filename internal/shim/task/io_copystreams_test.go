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
	closeCh := make(chan struct{})
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
	// now forces the select down the closeCh branch deterministically.
	<-r.started
	close(closeCh)

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
