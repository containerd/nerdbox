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
	"time"

	"github.com/containerd/fifo"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

func copyStreams(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) error {
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
			return err
		}
		var (
			fw io.WriteCloser
			fr io.Closer
		)
		if ok {
			// Open the write end probe-first so binding never blocks waiting
			// for the reader and fails fast if none appears, then open a
			// read-end holder so a later reader detach doesn't EOF our writer.
			if fw, err = openFifoWriteRendezvous(ctx, i.name); err != nil {
				return fmt.Errorf("containerd-shim: opening w/o fifo %q failed: %w", i.name, err)
			}
			if fr, err = fifo.OpenFifo(ctx, i.name, syscall.O_RDONLY, 0); err != nil {
				fw.Close()
				return fmt.Errorf("containerd-shim: opening r/o fifo %q failed: %w", i.name, err)
			}
		} else {
			if sameFile != nil {
				sameFile.bumpCount(1)
				i.dest(sameFile, nil)
				continue
			}
			if fw, err = os.OpenFile(i.name, syscall.O_WRONLY|syscall.O_APPEND, 0); err != nil {
				return fmt.Errorf("containerd-shim: opening file %q failed: %w", i.name, err)
			}
			if stdout == stderr {
				sameFile = newCountingWriteCloser(fw, 1)
			}
		}
		i.dest(fw, fr)
	}
	if stdin != "" {
		f, err := fifo.OpenFifo(context.Background(), stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return fmt.Errorf("containerd-shim: opening %s failed: %s", stdin, err)
		}
		cwg.Add(1)
		go func() {
			cwg.Done()
			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)

			io.CopyBuffer(streams[0], f, *p)
			streams[0].Close()
			f.Close()
		}()
	}
	cwg.Wait()
	return nil
}

const (
	// fifoRendezvousTimeout bounds how long binding waits for the reader to
	// open an output FIFO before giving up; the shim surfaces the failure
	// as DEADLINE_EXCEEDED.
	fifoRendezvousTimeout = 30 * time.Second
	// fifoRendezvousBackoff is the poll interval while no reader is present.
	fifoRendezvousBackoff = 10 * time.Millisecond
)

// openFifoWriteRendezvous opens the write end of a FIFO without blocking on a
// reader. open(O_WRONLY|O_NONBLOCK) returns ENXIO while no reader is open, so
// we poll until a reader appears, ctx is cancelled, or fifoRendezvousTimeout
// elapses — binding never stalls at start and fails fast if the reader never
// arrives. Once open, O_NONBLOCK is cleared so io.Copy's writes block on a
// full pipe (backpressure) instead of spinning on EAGAIN.
//
// O_NOFOLLOW guards client-supplied paths: a symlink swapped in after validation
// must not redirect the shim's writes to another file. The read-end holder and
// stdin instead go through containerd/fifo, which reopens by /proc/self/fd handle
// (itself a symlink, and already dev/ino-verified), so O_NOFOLLOW must not be set
// there - it would fail that reopen with ELOOP.
func openFifoWriteRendezvous(ctx context.Context, path string) (*os.File, error) {
	ctx, cancel := context.WithTimeout(ctx, fifoRendezvousTimeout)
	defer cancel()
	for {
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err == nil {
			// Clear O_NONBLOCK so io.Copy's writes block on a full pipe
			// (backpressure) instead of spinning on EAGAIN. If that fails,
			// fail the open and close the fd.
			fl, ferr := unix.FcntlInt(uintptr(fd), syscall.F_GETFL, 0)
			if ferr == nil {
				_, ferr = unix.FcntlInt(uintptr(fd), syscall.F_SETFL, fl&^syscall.O_NONBLOCK)
			}
			if ferr != nil {
				syscall.Close(fd)
				return nil, fmt.Errorf("clearing O_NONBLOCK on %q: %w", path, ferr)
			}
			return os.NewFile(uintptr(fd), path), nil
		}
		if err != syscall.ENXIO {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(fifoRendezvousBackoff):
		}
	}
}
