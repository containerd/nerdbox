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
	"io"
	"sync"

	"github.com/containerd/log"
)

// startStdinForward launches the goroutine that copies the stdin FIFO/pipe f to
// the guest over sc, and returns the stdinEOF callback CloseIO invokes to drain
// f and send the in-band EOF. It registers on cwg like the other stream copiers.
func startStdinForward(ctx context.Context, cwg *sync.WaitGroup, sc stdinStreamWriteCloser, f io.ReadCloser) func(context.Context) error {
	// closeCh delivers the CloseIO request context to the copy goroutine;
	// drainDone is closed once the in-band EOF has been sent.
	closeCh := make(chan context.Context, 1)
	drainDone := make(chan struct{})
	cwg.Add(1)
	go func() {
		cwg.Done()
		p := bufPool.Get().(*[]byte)
		defer bufPool.Put(p)
		copyStdinUntilClose(ctx, sc, f, *p, closeCh)
		// copyStdinUntilClose has delivered the in-band EOF (CloseWrite), so
		// signal CloseIO now rather than after f.Close, which could block.
		close(drainDone)
		// Do NOT Close sc here; ioShutdown/forwardIO owns the transport so it
		// outlives the in-band EOF and the host can close its end cleanly after
		// the guest drains. Closing f also releases any background read still
		// parked on it.
		f.Close()
	}()
	return stdinEOFFunc(closeCh, drainDone)
}

// stdinEOFFunc builds the stdinEOF callback invoked by CloseIO. It delivers the
// CloseIO request context to the copy goroutine on closeCh and blocks until the
// goroutine has sent the in-band EOF (drainDone) or the caller's context is
// done. Binding the drain to the CloseIO context means a peer that requests
// CloseIO without closing the FIFO write end cannot wedge CloseIO forever: the
// drain lasts only as long as the caller is willing to wait.
func stdinEOFFunc(closeCh chan<- context.Context, drainDone <-chan struct{}) func(context.Context) error {
	return func(cctx context.Context) error {
		// Non-blocking send so an already-cancelled cctx can't short-circuit
		// delivery and leave stdin open. closeCh is buffered (cap 1) and drained
		// at most once, so the request lands on the first CloseIO and is dropped
		// on a repeat or once the goroutine has exited (drainDone).
		select {
		case closeCh <- cctx:
		case <-drainDone:
			// Goroutine already exited (pipe/FIFO EOF): EOF already delivered.
			return nil
		default:
		}
		// cctx bounds only how long we wait for the drain, not delivery.
		select {
		case <-drainDone:
			return nil
		case <-cctx.Done():
			// drainDone and cctx.Done() can be ready together; prefer success so
			// a drain that finished isn't reported as a spurious CloseIO error.
			select {
			case <-drainDone:
				return nil
			default:
				return cctx.Err()
			}
		}
	}
}

// copyStdinUntilClose reads from f and writes raw bytes to sc until a CloseIO
// context arrives on closeCh (CloseIO) or f delivers EOF. On either exit it
// calls sc.CloseWrite() to send OP_SHUTDOWN(SEND) in-order on the vsock stdin
// stream, guaranteeing the guest sees EOF after all data already written —
// not via an out-of-band RPC that could race in-flight bytes.
//
// Reads always run on a background goroutine feeding readCh, so no read is ever
// performed synchronously in a select. That matters on the CloseIO drain path:
// draining to EOF requires the FIFO write end to be closed, but a peer can
// issue CloseIO without ever closing it. The drain is therefore bound to the
// CloseIO request context delivered on closeCh — if the caller cancels or its
// deadline elapses, the drain stops and still delivers the in-band EOF instead
// of wedging forever. On the conforming path (writer closes on CloseIO) the
// drain reads through to EOF with no data lost.
func copyStdinUntilClose(ctx context.Context, sc interface {
	io.Writer
	CloseWrite() error
}, f io.Reader, buf []byte, closeCh <-chan context.Context) {
	type readResult struct {
		n   int
		err error
	}
	readCh := make(chan readResult, 1)
	// read spawns a single background read into buf. At most one read is ever
	// in flight: the next read is only started after the current result has
	// been consumed and its bytes written, so buf is never shared concurrently.
	read := func() {
		go func() {
			n, err := f.Read(buf)
			readCh <- readResult{n, err}
		}()
	}
	closeWrite := func() {
		if err := sc.CloseWrite(); err != nil {
			log.G(ctx).WithError(err).Warn("error sending stdin EOF via CloseWrite")
		}
	}
	// writeChunk forwards buf[:res.n] to the guest, reporting a write error.
	writeChunk := func(res readResult) error {
		if res.n == 0 {
			return nil
		}
		_, err := sc.Write(buf[:res.n])
		return err
	}

	var (
		// closed is set once CloseIO fires; its Done() then bounds the drain so a
		// peer that requests CloseIO without closing the FIFO write end cannot wedge
		// us on a read that never completes.
		closed context.Context

		// abort set to closed.Done() once CloseIO fires.
		abort <-chan struct{}
	)

	read()
	for {
		if closed != nil {
			abort = closed.Done()
		}

		select {
		case closed = <-closeCh:
			// CloseIO fired: keep draining, now bounded by closed.Done().
			closeCh = nil
		case res := <-readCh:
			if err := writeChunk(res); err != nil {
				log.G(ctx).WithError(err).Warn("error writing stdin")
				closeWrite()
				return
			}
			// A read error (EOF: writer closed) always ends the copy; once
			// draining, a zero-length read means the FIFO is fully drained too.
			if res.err != nil || (closed != nil && res.n == 0) {
				closeWrite()
				return
			}
			read()
		case <-abort:
			// The CloseIO caller gave up before the FIFO delivered EOF: send the
			// in-band EOF rather than wedge. select is random, so abort can win
			// with a completed read's bytes still on readCh; a best-effort non-blocking
			// drain forwards those first (a still-in-flight read cannot be recovered
			// but is bound by fifo closure at the end of the copy goroutine).
			select {
			case res := <-readCh:
				if err := writeChunk(res); err != nil {
					log.G(ctx).WithError(err).Warn("error writing stdin on drain abort")
				}
			default:
			}
			log.G(ctx).WithError(closed.Err()).Warn("stdin drain aborted; CloseIO context done before FIFO EOF")
			closeWrite()
			return
		}
	}
}
