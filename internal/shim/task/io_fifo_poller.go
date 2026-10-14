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
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// This file holds the building blocks of copyStreamsPolled, the FIFO copy
// implementation used on darwin. They are plain POSIX code and are compiled
// (and unit tested) everywhere except Windows, but only darwin selects them in
// production; see startCopy.
//
// Why darwin needs its own FIFO handling
//
// os.OpenFile deliberately keeps FIFOs off the runtime poller on darwin
// ("kqueue does not work properly with fifos: closing the last writer does
// not cause a kqueue event for any readers", golang/go#24164). Every blocking
// FIFO Read or Write then parks an OS thread in read(2)/write(2), and Close
// cannot interrupt it: the descriptor is only released when the syscall
// returns on its own. On Linux the same FIFOs go through epoll and Close and
// deadlines work, so none of this is needed there.
//
// A descriptor that is already O_NONBLOCK and is handed to os.NewFile is
// registered with the poller even if it is a FIFO (the FIFO exclusion only
// applies to descriptors os.OpenFile opens itself), which restores Close and
// SetDeadline. The price is that the poller can miss exactly the event kqueue
// is unreliable about, the last writer closing. So nothing here may wait for
// the kernel to announce an end of file: the stdin pump polls for it instead
// (see stdinPump), and writers never need it.
//
// Opening non-blocking also removes the open(2) wakeup race of the same
// family: a reader asleep in a blocking open(2) can miss a writer that opens,
// writes and closes meanwhile, and the kernel then discards the bytes
// (containerd/fifo#63 describes the O_RDWR variant of the same workaround).
//
// This workaround can be removed once macOS delivers last-writer-close
// wakeups reliably to readers and Go registers FIFOs with kqueue on darwin
// again.

// openFifoNonblock opens the FIFO at path with the given access mode
// (syscall.O_RDONLY or syscall.O_WRONLY) in non-blocking mode and returns it
// as an *os.File registered with the runtime poller, so Close and the
// Set*Deadline methods interrupt a parked Read or Write.
//
// The open itself never blocks. A write-only open fails with ENXIO when no
// reader has the FIFO open; callers that need to wait for one use
// openFifoWriter.
func openFifoNonblock(path string, mode int) (*os.File, error) {
	var (
		fd  int
		err error
	)
	for {
		fd, err = syscall.Open(path, mode|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	// os.NewFile sees O_NONBLOCK and registers the descriptor with the poller.
	f := os.NewFile(uintptr(fd), path)
	// Registration can fail (for example if the descriptor is not something
	// the poller supports). A descriptor that is not registered would make
	// Read fail with EAGAIN and could not be interrupted, which is precisely
	// what this file exists to avoid, so refuse it up front.
	if err := f.SetDeadline(time.Time{}); err != nil {
		f.Close()
		return nil, &os.PathError{Op: "register", Path: path, Err: err}
	}
	return f, nil
}

const (
	fifoOpenRetryMin = time.Millisecond
	fifoOpenRetryMax = 50 * time.Millisecond
)

// openFifoWriter opens the write end of the FIFO at path, waiting for a reader
// to show up the way a blocking open(2) would, but without parking a thread in
// the kernel: a non-blocking open fails with ENXIO while the FIFO has no
// reader, so it is retried with a short, growing delay until ctx is done.
// Only the ENXIO "no reader yet" case is retried; any other error is returned.
//
// The wait matters: the client's reader must be attached before the shim
// writes, otherwise output written and closed before it attaches would be
// discarded by the kernel together with the FIFO's buffer.
func openFifoWriter(ctx context.Context, path string) (*os.File, error) {
	delay := fifoOpenRetryMin
	for {
		f, err := openFifoNonblock(path, syscall.O_WRONLY)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.ENXIO) {
			return nil, err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		delay = min(delay*2, fifoOpenRetryMax)
	}
}

// stdinEOFPollInterval bounds how long the stdin pump goes without trying
// read(2) once an end of input has been requested. It is the worst-case cost of
// a lost last-writer-close wakeup, and the CPU cost of waiting for a client
// that still holds its end open after CloseIO.
const stdinEOFPollInterval = 20 * time.Millisecond

// deadlineReader is the part of *os.File the stdin pump needs. The reader must
// support read deadlines, that is, be registered with the runtime poller.
type deadlineReader interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

// stdinPump forwards the read end of the stdin FIFO to the guest and finds the
// end of the input without relying on the kernel to announce it.
//
// src is the shim's read end of the FIFO and holder the shim's own write
// reference. While the holder is open, src cannot reach an end of file, since
// the FIFO always has a writer, so run simply waits for data in the poller and
// the client closing its own end is only a detach.
//
// requestEOF drops the holder. From then on the end of file is whatever
// read(2) says: it returns 0 once the client has closed its own end as well,
// after every buffered byte was returned. run does not wait for the kernel to
// announce that: it arms a short read deadline before every Read and retries
// when it expires, so a lost last-writer-close wakeup (golang/go#24164), which
// would otherwise park the reader forever, costs at most stdinEOFPollInterval.
// If the client still holds its write end open, run keeps forwarding what it
// writes until it closes.
type stdinPump struct {
	src    deadlineReader
	holder io.Closer
	// pollInterval overrides stdinEOFPollInterval when positive (tests).
	pollInterval time.Duration

	eofRequested atomic.Bool
	once         sync.Once
}

// requestEOF drops the holder so that the pump can reach a real end of input,
// and moves a pump that is parked waiting for data into polling mode. It is
// idempotent; later calls return nil.
func (p *stdinPump) requestEOF() error {
	var err error
	p.once.Do(func() {
		// The flag must be visible before the reader is interrupted: the
		// interruption surfaces as a deadline error that run only treats as
		// "poll again" because the flag is set.
		p.eofRequested.Store(true)
		err = p.holder.Close()
		if errors.Is(err, os.ErrClosed) {
			// Already released by a forced teardown; nothing left to do.
			err = nil
		}
		// Interrupt a Read parked without a deadline. This fails only if the
		// reader is already closed, which means the pump is done.
		_ = p.src.SetReadDeadline(time.Now())
	})
	return err
}

// run copies the stdin FIFO to dst until it reports a real end of file, or
// until reading or writing fails, and returns the error (nil for a clean end
// of file).
//
// Every Read is armed with a deadline in the future, never an expired one,
// because Read on an expired deadline returns os.ErrDeadlineExceeded without
// calling read(2) at all, which would drop buffered bytes.
func (p *stdinPump) run(dst io.Writer, buf []byte) error {
	interval := stdinEOFPollInterval
	if p.pollInterval > 0 {
		interval = p.pollInterval
	}
	for {
		if p.eofRequested.Load() {
			if err := p.src.SetReadDeadline(time.Now().Add(interval)); err != nil {
				return err
			}
		}
		n, err := p.src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			if werr == nil && w < n {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				return werr
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, os.ErrDeadlineExceeded):
			// Either the poll interval elapsed or requestEOF interrupted a
			// parked Read; the next iteration re-arms and tries read(2).
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}
