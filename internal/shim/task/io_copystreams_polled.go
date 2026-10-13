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

// writerOnly hides any io.ReaderFrom the wrapped writer implements, so
// io.CopyBuffer keeps copying through the pooled buffer and in the same
// chunk size as the Linux implementation does.
type writerOnly struct{ io.Writer }

// copyStreamsPolled is the darwin implementation of copyStreams (see
// startCopy); it is compiled everywhere but Windows so its logic is also
// exercised on Linux. It has the same contract as the Linux copyStreams in
// io_copystreams_unix.go, which is the reference for the documentation of
// stdinEOF and stdinDone:
//
//   - the host's O_WRONLY reference on the stdin FIFO is what turns a client
//     closing its own write end into a detach rather than an end of input;
//     stdinEOF (CloseIO, or container teardown) drops it;
//   - after that the stdin goroutine still forwards everything buffered, and
//     everything the client still writes until it closes its own end, in
//     order, and only then sends the in-band CloseWrite and closes stdinDone.
//
// What differs is how the FIFO descriptors are obtained, see
// io_fifo_poller.go: they are all non-blocking and registered with the runtime
// poller, so no goroutine pins an OS thread in read(2)/write(2) and closing a
// descriptor interrupts it. The end of the stdin FIFO is found by polling
// (stdinPump) instead of waiting for a wakeup macOS may not deliver.
//
// abort, when called, closes every FIFO descriptor this call opened and
// thereby releases any copy goroutine parked on one, a stdin reader waiting
// for a client that never closes its end or an output writer blocked because
// the client stopped reading. It is the force-close the bounded waits in
// ioShutdown fall back to. An aborted stdin never sends the in-band EOF, so a
// truncated input does not look like a clean end of input to the process.
// abort is idempotent and may be called after the copy has completed.
func copyStreamsPolled(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) (stdinEOF func() error, stdinDone <-chan struct{}, abort func(), err error) {
	var (
		cwg      sync.WaitGroup
		copying  atomic.Int32
		aborted  atomic.Bool
		sameFile *countingWriteCloser
		// fifos collects every descriptor abort has to close. It is only
		// appended to before this function returns.
		fifos []io.Closer
	)
	copying.Store(2)
	closeFifos := func() {
		for _, c := range fifos {
			c.Close()
		}
	}
	defer func() {
		if err != nil {
			// Do not leave descriptors (and the goroutines parked on them)
			// behind when setting up a later stream failed.
			aborted.Store(true)
			closeFifos()
		}
	}()

	startOutput := func(name string, src io.Reader, wc io.WriteCloser, rc io.Closer) {
		cwg.Add(1)
		go func() {
			cwg.Done()
			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)
			if _, err := io.CopyBuffer(writerOnly{wc}, src, *p); err != nil && !aborted.Load() {
				log.G(ctx).WithError(err).Warnf("error copying %s", name)
			}
			if copying.Add(-1) == 0 {
				close(done)
			}
			wc.Close()
			if rc != nil {
				rc.Close()
			}
		}()
	}

	for _, i := range []struct {
		name string
		kind string
		src  io.Reader
	}{
		{name: stdout, kind: "stdout", src: streams[1]},
		{name: stderr, kind: "stderr", src: streams[2]},
	} {
		if i.name == "" {
			if copying.Add(-1) == 0 {
				close(done)
			}
			continue
		}
		ok, err := fifo.IsFifo(i.name)
		if err != nil {
			return nil, nil, nil, err
		}
		if !ok {
			// A regular file can neither block nor be left unread, so none of
			// the FIFO handling applies. This is identical to the Linux
			// implementation.
			if sameFile != nil {
				sameFile.bumpCount(1)
				startOutput(i.kind, i.src, sameFile, nil)
				continue
			}
			fw, err := os.OpenFile(i.name, syscall.O_WRONLY|syscall.O_APPEND, 0)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("containerd-shim: opening file %q failed: %w", i.name, err)
			}
			if stdout == stderr {
				sameFile = newCountingWriteCloser(fw, 1)
			}
			startOutput(i.kind, i.src, fw, nil)
			continue
		}

		// Same two references as the Linux implementation: the writer, which
		// waits for the client's reader, then a read reference that keeps the
		// FIFO free of EPIPE if the client closes its end first.
		fw, err := openFifoWriter(ctx, i.name)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("containerd-shim: opening w/o fifo %q failed: %w", i.name, err)
		}
		fifos = append(fifos, fw)
		fr, err := openFifoNonblock(i.name, syscall.O_RDONLY)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("containerd-shim: opening r/o fifo %q failed: %w", i.name, err)
		}
		fifos = append(fifos, fr)
		startOutput(i.kind, i.src, fw, fr)
	}

	if stdin != "" {
		// Assert early, see the Linux implementation.
		sc, ok := streams[0].(stdinStreamWriteCloser)
		if !ok {
			return nil, nil, nil, fmt.Errorf("stdin stream connection does not implement CloseWrite; vsock conn required")
		}

		// The order is the point. The non-blocking read open always succeeds
		// and leaves a reader on the FIFO, which is what lets the
		// non-blocking write open that follows succeed too, so neither open
		// can sleep in the kernel or miss the other side. From here on the
		// holder is a writer for as long as it stays open, so the reader
		// cannot see an end of file, and the client closing its own write end
		// is only a detach, until stdinEOF drops it.
		rd, err := openFifoNonblock(stdin, syscall.O_RDONLY)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("containerd-shim: opening %s failed: %w", stdin, err)
		}
		fifos = append(fifos, rd)
		hold, err := openFifoNonblock(stdin, syscall.O_WRONLY)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("containerd-shim: opening w/o stdin fifo %q failed: %w", stdin, err)
		}
		fifos = append(fifos, hold)

		pump := &stdinPump{src: rd, holder: hold}
		stdinDoneCh := make(chan struct{})
		stdinDone = stdinDoneCh
		cwg.Add(1)
		go func() {
			cwg.Done()
			defer close(stdinDoneCh)
			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)
			// Drains to a real end of file: it only returns once every
			// writer, the holder and the client, has gone, and every byte
			// buffered before that has been forwarded to the guest.
			err := pump.run(sc, *p)
			if aborted.Load() {
				// Forced teardown: the input is truncated, so no EOF.
				rd.Close()
				return
			}
			if err != nil {
				log.G(ctx).WithError(err).Warn("error copying stdin")
			}
			// All buffered bytes are now on the wire; deliver EOF in-band.
			if err := sc.CloseWrite(); err != nil {
				log.G(ctx).WithError(err).Warn("error sending stdin EOF via CloseWrite")
			}
			// Do NOT Close sc here; deferred to ioShutdown/forwardIO cleanup
			// so the transport outlives the in-band EOF.
			rd.Close()
		}()
		// Idempotent: called by CloseIO and again by container teardown as a
		// safety net.
		stdinEOF = pump.requestEOF
	}
	cwg.Wait()
	abort = func() {
		aborted.Store(true)
		closeFifos()
	}
	return stdinEOF, stdinDone, abort, nil
}
