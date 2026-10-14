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
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// kernelReader models the read end of a FIFO the way the Go poller exposes it,
// including the macOS misbehaviour the stdin pump has to survive:
//
//   - a Read first checks its deadline and fails with os.ErrDeadlineExceeded
//     without looking at the FIFO if it has expired (what os.File does: the
//     poller refuses before read(2));
//   - otherwise it behaves like read(2): buffered bytes first, then 0 (EOF) once
//     no writer is left, otherwise it parks;
//   - a parked Read is woken by data, by its deadline and by Close, but NOT by
//     the last writer going away: that wakeup is the one that gets lost.
type kernelReader struct {
	mu        sync.Mutex
	data      []byte
	noWriters bool
	deadline  time.Time
	closed    bool

	wake  chan struct{} // data arrived or deadline changed or closed
	reads atomic.Int64  // completed Read calls
	// interrupts counts deadlines set to a time that has already passed.
	interrupts atomic.Int64
	parkedCh   chan struct{} // signalled each time a Read parks
}

func newKernelReader() *kernelReader {
	return &kernelReader{wake: make(chan struct{}, 1), parkedCh: make(chan struct{}, 1)}
}

func (k *kernelReader) poke() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

func (k *kernelReader) push(b string) {
	k.mu.Lock()
	k.data = append(k.data, b...)
	k.mu.Unlock()
	k.poke()
}

// lastWriterClosed records the kernel state "no writers left" and, like the
// lost wakeup, does not wake a parked Read.
func (k *kernelReader) lastWriterClosed() {
	k.mu.Lock()
	k.noWriters = true
	k.mu.Unlock()
}

func (k *kernelReader) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		k.interrupts.Add(1)
	}
	k.mu.Lock()
	closed := k.closed
	k.deadline = t
	k.mu.Unlock()
	if closed {
		return os.ErrClosed
	}
	k.poke()
	return nil
}

func (k *kernelReader) Close() error {
	k.mu.Lock()
	k.closed = true
	k.mu.Unlock()
	k.poke()
	return nil
}

func (k *kernelReader) Read(p []byte) (int, error) {
	defer k.reads.Add(1)
	for {
		k.mu.Lock()
		switch {
		case k.closed:
			k.mu.Unlock()
			return 0, os.ErrClosed
		case !k.deadline.IsZero() && !time.Now().Before(k.deadline):
			k.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		case len(k.data) > 0:
			n := copy(p, k.data)
			k.data = k.data[n:]
			k.mu.Unlock()
			return n, nil
		case k.noWriters:
			k.mu.Unlock()
			return 0, io.EOF
		}
		deadline := k.deadline
		k.mu.Unlock()

		select {
		case k.parkedCh <- struct{}{}:
		default:
		}
		var expired <-chan time.Time
		if !deadline.IsZero() {
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			expired = timer.C
		}
		select {
		case <-k.wake:
		case <-expired:
		}
	}
}

// fakeHolder is the shim's own write reference. Closing it records whether the
// pump had already been told that EOF is wanted, which is the order that
// matters.
type fakeHolder struct {
	pump                  *stdinPump
	closes                atomic.Int32
	flagWasSet            atomic.Bool
	closeErr              error
	closedBeforeInterrupt atomic.Bool // set if closed after the reader was interrupted
	reader                *kernelReader
}

func (h *fakeHolder) Close() error {
	h.closes.Add(1)
	h.flagWasSet.Store(h.pump.eofRequested.Load())
	h.closedBeforeInterrupt.Store(h.reader.interrupts.Load() == 0)
	return h.closeErr
}

// testPollInterval keeps the polling tests fast.
const testPollInterval = 5 * time.Millisecond

func newTestPump(k *kernelReader) (*stdinPump, *fakeHolder) {
	h := &fakeHolder{reader: k}
	p := &stdinPump{src: k, holder: h, pollInterval: testPollInterval}
	h.pump = p
	return p, h
}

type pumpResult struct {
	err error
	out string
}

func runPump(p *stdinPump) <-chan pumpResult {
	res := make(chan pumpResult, 1)
	go func() {
		var out bytes.Buffer
		err := p.run(&out, make([]byte, 4096))
		res <- pumpResult{err, out.String()}
	}()
	return res
}

func waitPump(t *testing.T, res <-chan pumpResult) pumpResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(testWait):
		t.Fatal("the stdin pump did not return")
		return pumpResult{}
	}
}

// TestStdinPumpRecoversLostLastWriterWakeup: the client closes its end after
// CloseIO and the kernel records the end of file, but the parked reader is not
// woken. The pump must find it by itself on its next poll.
//
// Without the periodic re-arm the pump never sees the end of file and this
// test times out.
func TestStdinPumpRecoversLostLastWriterWakeup(t *testing.T) {
	k := newKernelReader()
	p, _ := newTestPump(k)
	res := runPump(p)

	k.push("abc")
	// Wait until the pump has relayed it and parked again.
	for k.reads.Load() < 1 {
		<-k.parkedCh
	}

	require.NoError(t, p.requestEOF()) // CloseIO; the client still has its end open
	k.lastWriterClosed()               // the client closes: no wakeup

	r := waitPump(t, res)
	require.NoError(t, r.err)
	require.Equal(t, "abc", r.out)
}

// TestStdinPumpRequestEOFInterruptsParkedRead: a pump parked without a deadline
// must be moved into polling by requestEOF itself; without that interruption it
// would wait for a wakeup that is allowed to never come.
func TestStdinPumpRequestEOFInterruptsParkedRead(t *testing.T) {
	k := newKernelReader()
	p, h := newTestPump(k)
	res := runPump(p)
	<-k.parkedCh // parked in Read with no deadline

	// The end of file is already there, and nothing will wake the pump.
	k.lastWriterClosed()
	require.NoError(t, p.requestEOF())

	r := waitPump(t, res)
	require.NoError(t, r.err)
	require.Equal(t, int32(1), h.closes.Load())
	require.True(t, h.flagWasSet.Load(), "holder closed before the pump was told EOF is wanted")
	require.True(t, h.closedBeforeInterrupt.Load(), "reader was interrupted before the holder was closed")
}

// TestStdinPumpKeepsBufferedBytesOnInterrupt: bytes buffered when the pump is
// interrupted must be forwarded before the end of file. A Read on an expired
// deadline never looks at the FIFO, so a pump that drained with the
// interrupting deadline would drop them.
func TestStdinPumpKeepsBufferedBytesOnInterrupt(t *testing.T) {
	k := newKernelReader()
	k.push("buffered ")
	k.push("bytes")
	k.lastWriterClosed()
	p, _ := newTestPump(k)
	require.NoError(t, p.requestEOF()) // before the pump even starts

	r := waitPump(t, runPump(p))
	require.NoError(t, r.err)
	require.Equal(t, "buffered bytes", r.out)
}

// TestStdinPumpRequestEOFIdempotent: the holder is dropped once, and the
// first call's error is the one reported.
func TestStdinPumpRequestEOFIdempotent(t *testing.T) {
	k := newKernelReader()
	p, h := newTestPump(k)
	h.closeErr = io.ErrClosedPipe
	require.ErrorIs(t, p.requestEOF(), io.ErrClosedPipe)
	require.NoError(t, p.requestEOF())
	require.NoError(t, p.requestEOF())
	require.Equal(t, int32(1), h.closes.Load())
}

// TestStdinPumpRequestEOFAfterForcedClose: the holder may already have been
// closed by a forced teardown; CloseIO arriving afterwards is not an error.
func TestStdinPumpRequestEOFAfterForcedClose(t *testing.T) {
	k := newKernelReader()
	p, h := newTestPump(k)
	h.closeErr = &os.PathError{Op: "close", Path: "stdin", Err: os.ErrClosed}
	require.NoError(t, p.requestEOF())
}

// TestStdinPumpErrors: a failing destination and a closed reader end the pump
// with the error, which is what the copy goroutine reports.
func TestStdinPumpErrors(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		k := newKernelReader()
		k.push("x")
		p, _ := newTestPump(k)
		require.ErrorIs(t, p.run(failingWriter{}, make([]byte, 16)), io.ErrClosedPipe)
	})
	t.Run("short write", func(t *testing.T) {
		k := newKernelReader()
		k.push("xyz")
		p, _ := newTestPump(k)
		require.ErrorIs(t, p.run(shortWriter{}, make([]byte, 16)), io.ErrShortWrite)
	})
	t.Run("closed", func(t *testing.T) {
		k := newKernelReader()
		p, _ := newTestPump(k)
		res := runPump(p)
		<-k.parkedCh
		k.Close()
		require.ErrorIs(t, waitPump(t, res).err, os.ErrClosed)
	})
	t.Run("closed while polling", func(t *testing.T) {
		k := newKernelReader()
		p, _ := newTestPump(k)
		res := runPump(p)
		<-k.parkedCh
		require.NoError(t, p.requestEOF())
		k.Close()
		require.ErrorIs(t, waitPump(t, res).err, os.ErrClosed)
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

// TestOpenFifoNonblockIsInterruptible: the descriptors the polled
// implementation uses are registered with the runtime poller, so a deadline
// and Close interrupt a parked Read. This is the property os.OpenFile does not
// give for FIFOs on darwin; on macOS CI this runs against kqueue.
func TestOpenFifoNonblockIsInterruptible(t *testing.T) {
	path := mkfifo(t, "fifo")
	// O_RDWR so the FIFO has a writer and Read waits instead of returning EOF.
	f, err := openFifoNonblock(path, syscall.O_RDWR)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })

	require.NoError(t, f.SetReadDeadline(time.Now().Add(10*time.Millisecond)))
	_, err = f.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.NoError(t, f.SetReadDeadline(time.Time{}))

	res := make(chan error, 1)
	go func() {
		_, err := f.Read(make([]byte, 1))
		res <- err
	}()
	require.NoError(t, f.Close()) // may run before or after the Read parks
	select {
	case err := <-res:
		require.ErrorIs(t, err, os.ErrClosed)
	case <-time.After(testWait):
		t.Fatal("Close did not interrupt a Read parked on the FIFO")
	}
}

// TestOpenFifoNonblockWriteInterruptible: the same for a Write blocked on a
// full FIFO.
func TestOpenFifoNonblockWriteInterruptible(t *testing.T) {
	path := mkfifo(t, "fifo")
	r, err := openFifoNonblock(path, syscall.O_RDONLY)
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	w, err := openFifoNonblock(path, syscall.O_WRONLY)
	require.NoError(t, err)
	t.Cleanup(func() { w.Close() })

	res := make(chan error, 1)
	go func() {
		// Far more than any FIFO holds; nobody reads.
		_, err := w.Write(make([]byte, 8<<20))
		res <- err
	}()
	fillFifo(t, path) // the goroutine's Write cannot complete once this returns
	require.NoError(t, w.Close())
	select {
	case err := <-res:
		require.ErrorIs(t, err, os.ErrClosed)
	case <-time.After(testWait):
		t.Fatal("Close did not interrupt a Write blocked on a full FIFO")
	}
}

// TestOpenFifoWriterWaitsForReader: like a blocking open, the writer open
// waits for a reader to appear.
func TestOpenFifoWriterWaitsForReader(t *testing.T) {
	path := mkfifo(t, "fifo")
	type result struct {
		f   *os.File
		err error
	}
	res := make(chan result, 1)
	ctx, cancel := context.WithTimeout(t.Context(), testWait)
	defer cancel()
	go func() {
		f, err := openFifoWriter(ctx, path)
		res <- result{f, err}
	}()
	// Whether the writer is already retrying or not does not matter.
	r, err := openFifoNonblock(path, syscall.O_RDONLY)
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	select {
	case got := <-res:
		require.NoError(t, got.err)
		require.NoError(t, got.f.Close())
	case <-time.After(testWait):
		t.Fatal("writer open never saw the reader")
	}
}

// TestOpenFifoWriterPairsWithReaderBlockedInOpen: a client that opened the read
// end with a blocking open(2), as containerd's fifo does in the background,
// counts as a reader: the non-blocking writer open must succeed while that open
// is still asleep, and wake it. Both sides would otherwise wait for the other
// forever. On macOS CI this pins the behaviour of the real kernel.
func TestOpenFifoWriterPairsWithReaderBlockedInOpen(t *testing.T) {
	path := mkfifo(t, "fifo")
	type result struct {
		f   *os.File
		err error
	}
	clientOpen := make(chan result, 1)
	go func() {
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		clientOpen <- result{f, err}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), testWait)
	defer cancel()
	w, err := openFifoWriter(ctx, path)
	require.NoError(t, err, "the writer open never saw the reader that is blocked in open(2)")
	t.Cleanup(func() { w.Close() })
	select {
	case got := <-clientOpen:
		require.NoError(t, got.err)
		defer got.f.Close()
		_, err = w.Write([]byte("data"))
		require.NoError(t, err)
		buf := make([]byte, 4)
		_, err = io.ReadFull(got.f, buf)
		require.NoError(t, err)
		require.Equal(t, "data", string(buf))
	case <-time.After(testWait):
		t.Fatal("the client's blocking open was not woken by the writer open")
	}
}

// TestOpenFifoWriterCanceled: with no reader the wait ends with the context
// instead of parking anything.
func TestOpenFifoWriterCanceled(t *testing.T) {
	path := mkfifo(t, "fifo")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := openFifoWriter(ctx, path)
	require.ErrorIs(t, err, context.Canceled)
}

// TestOpenFifoWriterOtherErrors: only "no reader yet" is retried.
func TestOpenFifoWriterOtherErrors(t *testing.T) {
	_, err := openFifoWriter(t.Context(), mkfifo(t, "fifo")+".missing")
	require.ErrorIs(t, err, os.ErrNotExist)
}
