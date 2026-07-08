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

// copyStreams returns a stdinEOF function that, when called (by CloseIO with
// the CloseIO request context), drains the stdin FIFO and sends the
// OP_SHUTDOWN(SEND) in-band EOF to the guest. It blocks until the EOF has been
// delivered or the passed context is cancelled, binding the drain to the
// CloseIO RPC. It is nil when stdin is empty.
func copyStreams(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) (stdinEOF func(context.Context) error, err error) {
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
						log.G(ctx).WithError(err).WithField("stream", streams[1]).Warn("error copying stdout")
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
			return nil, err
		}
		var (
			fw io.WriteCloser
			fr io.Closer
		)
		if ok {
			if fw, err = fifo.OpenFifo(ctx, i.name, syscall.O_WRONLY, 0); err != nil {
				return nil, fmt.Errorf("containerd-shim: opening w/o fifo %q failed: %w", i.name, err)
			}
			if fr, err = fifo.OpenFifo(ctx, i.name, syscall.O_RDONLY, 0); err != nil {
				return nil, fmt.Errorf("containerd-shim: opening r/o fifo %q failed: %w", i.name, err)
			}
		} else {
			if sameFile != nil {
				sameFile.bumpCount(1)
				i.dest(sameFile, nil)
				continue
			}
			if fw, err = os.OpenFile(i.name, syscall.O_WRONLY|syscall.O_APPEND, 0); err != nil {
				return nil, fmt.Errorf("containerd-shim: opening file %q failed: %w", i.name, err)
			}
			if stdout == stderr {
				sameFile = newCountingWriteCloser(fw, 1)
			}
		}
		i.dest(fw, fr)
	}
	if stdin != "" {
		// Assert early: the stdin vsock stream must implement CloseWrite so
		// we can send OP_SHUTDOWN(SEND) in-order when CloseIO fires, rather
		// than forwarding an out-of-band RPC that races in-flight bytes.
		sc, ok := streams[0].(stdinStreamWriteCloser)
		if !ok {
			return nil, fmt.Errorf("stdin stream connection does not implement CloseWrite; vsock conn required")
		}
		f, err := fifo.OpenFifo(context.Background(), stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("containerd-shim: opening %s failed: %s", stdin, err)
		}
		stdinEOF = startStdinForward(ctx, &cwg, sc, f)
	}
	cwg.Wait()
	return stdinEOF, nil
}
