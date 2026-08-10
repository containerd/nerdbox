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

package task

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/containerd/fifo"
	"github.com/containerd/log"
)

// stdinStreamWriteCloser is the interface the host-side stdin vsock stream
// connection must implement. CloseWrite sends OP_SHUTDOWN(SEND) in-order
// after all data, delivering EOF to the guest without a destructive transport
// close. Asserting at setup time ensures a future wrapper that drops
// CloseWrite fails loudly rather than silently hanging the guest's read.
type stdinStreamWriteCloser interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// copyStreams returns a stdinEOF function and a stdinDone channel for the
// stdin FIFO (both nil when stdin is empty).
//
// stdinEOF, when called (by CloseIO or container teardown), drops the
// host's own O_WRONLY reference on the stdin FIFO -- mirroring the
// reference containerd runc shim's stdin FIFO handling. Holding that
// reference is what lets the external client close its own FIFO write end
// to detach without delivering EOF to the process: the FIFO can only reach
// a real EOF once every writer, including this one, is closed. Dropping it
// does not force anything; the copy goroutine still fully drains whatever
// is already buffered in the FIFO (and whatever the client still writes,
// if it hasn't closed its own end yet) before the guest sees EOF.
//
// stdinDone is closed once the copy goroutine has drained the FIFO to a
// real EOF and delivered the in-band CloseWrite, so callers can wait for
// it before tearing down the underlying stream connection.
func copyStreams(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) (stdinEOF func() error, stdinDone <-chan struct{}, err error) {
	var cwg sync.WaitGroup
	var copying atomic.Int32
	copying.Store(2)
	var sameFile *countingWriteCloser
	for _, i := range []struct {
		name string
		dest func(wc io.WriteCloser, rc io.Closer)
	}{
		{
			name: stdout,
			dest: func(wc io.WriteCloser, rc io.Closer) {
				cwg.Add(1)
				go func() {
					cwg.Done()
					p := bufPool.Get().(*[]byte)
					defer bufPool.Put(p)
					if _, err := io.CopyBuffer(wc, streams[1], *p); err != nil {
						log.G(ctx).WithError(err).WithField("stream_id", streams[1]).Warn("error copying stdout")
					}
					if copying.Add(-1) == 0 {
						close(done)
					}
					wc.Close()
					if rc != nil {
						rc.Close()
					}
				}()
			},
		}, {
			name: stderr,
			dest: func(wc io.WriteCloser, rc io.Closer) {
				cwg.Add(1)
				go func() {
					cwg.Done()
					p := bufPool.Get().(*[]byte)
					defer bufPool.Put(p)
					if _, err := io.CopyBuffer(wc, streams[2], *p); err != nil {
						log.G(ctx).WithError(err).Warn("error copying stderr")
					}
					if copying.Add(-1) == 0 {
						close(done)
					}
					wc.Close()
					if rc != nil {
						rc.Close()
					}
				}()
			},
		},
	} {
		if i.name == "" {
			if copying.Add(-1) == 0 {
				close(done)
			}
			continue
		}
		ok, err := fifo.IsFifo(i.name)
		if err != nil {
			return nil, nil, err
		}
		var (
			fw io.WriteCloser
			fr io.Closer
		)
		if ok {
			if fw, err = fifo.OpenFifo(ctx, i.name, syscall.O_WRONLY, 0); err != nil {
				return nil, nil, fmt.Errorf("containerd-shim: opening w/o fifo %q failed: %w", i.name, err)
			}
			if fr, err = fifo.OpenFifo(ctx, i.name, syscall.O_RDONLY, 0); err != nil {
				return nil, nil, fmt.Errorf("containerd-shim: opening r/o fifo %q failed: %w", i.name, err)
			}
		} else {
			if sameFile != nil {
				sameFile.bumpCount(1)
				i.dest(sameFile, nil)
				continue
			}
			if fw, err = os.OpenFile(i.name, syscall.O_WRONLY|syscall.O_APPEND, 0); err != nil {
				return nil, nil, fmt.Errorf("containerd-shim: opening file %q failed: %w", i.name, err)
			}
			if stdout == stderr {
				sameFile = newCountingWriteCloser(fw, 1)
			}
		}
		i.dest(fw, fr)
	}
	if stdin != "" {
		// Assert early: the stdin vsock stream must implement CloseWrite so
		// we can send OP_SHUTDOWN(SEND) in-order once the FIFO reaches a
		// real EOF, rather than forwarding an out-of-band RPC that races
		// in-flight bytes.
		sc, ok := streams[0].(stdinStreamWriteCloser)
		if !ok {
			return nil, nil, fmt.Errorf("stdin stream connection does not implement CloseWrite; vsock conn required")
		}

		// Hold our own O_WRONLY reference on the stdin FIFO, mirroring the
		// reference containerd runc shim's stdin handling (it opens the
		// FIFO write end itself to unblock its own O_RDONLY open and to
		// decouple client detach from process EOF). As long as this
		// reference is open, the FIFO cannot reach EOF even if the
		// external client closes its own write end -- that just means
		// detach. EOF is only delivered once this reference is dropped by
		// stdinEOF below (CloseIO or container teardown).
		fw, err := fifo.OpenFifo(context.Background(), stdin, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("containerd-shim: opening w/o stdin fifo %q failed: %w", stdin, err)
		}
		f, err := fifo.OpenFifo(context.Background(), stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			fw.Close()
			return nil, nil, fmt.Errorf("containerd-shim: opening %s failed: %s", stdin, err)
		}
		stdinDoneCh := make(chan struct{})
		stdinDone = stdinDoneCh
		cwg.Add(1)
		go func() {
			cwg.Done()
			defer close(stdinDoneCh)
			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)
			// Drain to a real EOF: io.CopyBuffer only returns once every
			// writer of the FIFO -- including our own reference above --
			// has closed, guaranteeing every byte buffered before EOF has
			// already been forwarded to the guest via sc.Write.
			if _, err := io.CopyBuffer(sc, f, *p); err != nil {
				log.G(ctx).WithError(err).Warn("error copying stdin")
			}
			// All buffered bytes are now on the wire; deliver EOF in-band.
			if err := sc.CloseWrite(); err != nil {
				log.G(ctx).WithError(err).Warn("error sending stdin EOF via CloseWrite")
			}
			// Do NOT Close sc here; deferred to ioShutdown/forwardIO cleanup
			// so the transport outlives the in-band EOF and the host can
			// close its end cleanly after the guest drains.
			f.Close()
		}()
		stdinEOF = func() error {
			// Drop our write reference. Idempotent: fifo.Close is safe to
			// call more than once (e.g. once from an explicit CloseIO
			// call and again from container teardown as a safety net for
			// callers that never issue CloseIO).
			return fw.Close()
		}
	}
	cwg.Wait()
	return stdinEOF, stdinDone, nil
}
