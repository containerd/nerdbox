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
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/stretchr/testify/require"
)

// These tests drive the stdio copy with real FIFOs and an in-memory stand-in
// for the vsock streams, the way io_copystreams_windows_test.go does for named
// pipes. They run wherever the FIFO code is compiled (everything but Windows)
// so the logic is exercised on Linux CI as well; on macOS CI the very same
// tests run against the real kqueue poller. Outcomes never depend on elapsed
// time: every wait is for a state, with a generous timeout that only bounds a
// failing run.

const testWait = 20 * time.Second

// copyImpl is one implementation of the stdio copy.
type copyImpl struct {
	name string
	fn   copyStreamsFunc
}

var (
	// contractImpls must all honour the same stdin/stdout contract. "platform"
	// is what the shim runs on this OS, which on Linux is the original
	// copyStreams, so the suite pins the contract the polled implementation
	// has to preserve. On darwin both entries are the polled implementation.
	contractImpls = []copyImpl{
		{"polled", copyStreamsPolled},
		{"platform", startCopy},
	}
	// teardownImpls must additionally be able to force-release goroutines that
	// are parked on a FIFO from ioShutdown. The platform implementation joins
	// the list on darwin only (io_copystreams_darwin_test.go): on Linux the
	// original copyStreams does not have that ability and is not changed.
	teardownImpls = []copyImpl{
		{"polled", copyStreamsPolled},
	}
)

// fakeConn stands in for a vsock stream connection: the host reads what the
// test plays as the guest's output (guestOut), and everything the host writes
// is recorded in order together with whether the in-band end of input
// (CloseWrite) was sent.
type fakeConn struct {
	net.Conn // unused, only so a *fakeConn is a net.Conn for streamCreator

	guestOut *io.PipeWriter
	hostIn   *io.PipeReader

	mu      sync.Mutex
	sent    []byte
	eof     bool
	late    bool // bytes written after CloseWrite
	closed  bool
	changed chan struct{}
}

func newFakeConn() *fakeConn {
	r, w := io.Pipe()
	return &fakeConn{guestOut: w, hostIn: r, changed: make(chan struct{}, 1)}
}

func (c *fakeConn) signal() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *fakeConn) Read(p []byte) (int, error) { return c.hostIn.Read(p) }

func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.eof {
		c.late = true
	}
	c.sent = append(c.sent, p...)
	c.signal()
	return len(p), nil
}

func (c *fakeConn) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eof = true
	c.signal()
	return nil
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.hostIn.CloseWithError(net.ErrClosed)
	c.guestOut.CloseWithError(net.ErrClosed)
	c.signal()
	return nil
}

func (c *fakeConn) snapshot() (sent []byte, eof, late bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.sent), c.eof, c.late
}

// waitFor blocks until cond holds, re-evaluating it after every change
// signalled on c.
func (c *fakeConn) waitFor(t *testing.T, what string, cond func(sent []byte, eof bool) bool) {
	t.Helper()
	deadline := time.NewTimer(testWait)
	defer deadline.Stop()
	for {
		sent, eof, _ := c.snapshot()
		if cond(sent, eof) {
			return
		}
		select {
		case <-c.changed:
		case <-deadline.C:
			sent, eof, _ := c.snapshot()
			t.Fatalf("timed out waiting for %s (relayed %d bytes, eof=%v)", what, len(sent), eof)
		}
	}
}

func (c *fakeConn) waitSent(t *testing.T, want []byte) {
	t.Helper()
	c.waitFor(t, fmt.Sprintf("%d relayed stdin bytes", len(want)), func(sent []byte, _ bool) bool {
		return len(sent) >= len(want)
	})
	sent, _, _ := c.snapshot()
	require.Equal(t, len(want), len(sent), "unexpected extra stdin bytes")
	require.True(t, bytes.Equal(want, sent), "relayed stdin bytes differ")
}

func mkfifo(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, syscall.Mkfifo(p, 0o600))
	return p
}

// openClientEnd opens one end of the FIFO as an external client would,
// non-blocking and on the poller. A write-only open fails with ENXIO until the
// shim has a reader open, so that case is retried.
func openClientEnd(t *testing.T, path string, mode int) *os.File {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		fd, err := syscall.Open(path, mode|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err == nil {
			f := os.NewFile(uintptr(fd), path)
			t.Cleanup(func() { f.Close() })
			return f
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.ENXIO) || time.Now().After(deadline) {
			t.Fatalf("opening client end of %s: %v", path, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// readToEOF reads f until end of file. Each Read is armed with a short
// deadline and retried, so that the test does not itself rely on the kernel
// announcing the last writer's close, which is the thing the shim must not
// rely on either.
func readToEOF(t *testing.T, f *os.File) []byte {
	t.Helper()
	var (
		out      bytes.Buffer
		buf      = make([]byte, 4096)
		deadline = time.Now().Add(testWait)
	)
	for {
		require.NoError(t, f.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
		n, err := f.Read(buf)
		out.Write(buf[:n])
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return out.Bytes()
		case errors.Is(err, os.ErrDeadlineExceeded):
			if time.Now().After(deadline) {
				t.Fatalf("timed out reading %s to EOF (got %d bytes)", f.Name(), out.Len())
			}
		default:
			t.Fatalf("reading %s: %v", f.Name(), err)
		}
	}
}

func chanClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// stdinCase is one running stdin copy.
type stdinCase struct {
	path      string
	conn      *fakeConn
	stdinEOF  func() error
	stdinDone <-chan struct{}
	abort     func()
}

func startStdin(t *testing.T, impl copyImpl) *stdinCase {
	t.Helper()
	c := &stdinCase{path: mkfifo(t, "stdin"), conn: newFakeConn()}
	ioDone := make(chan struct{})
	var err error
	c.stdinEOF, c.stdinDone, c.abort, err = impl.fn(t.Context(), [3]io.ReadWriteCloser{c.conn, nil, nil}, c.path, "", "", ioDone)
	require.NoError(t, err)
	require.NotNil(t, c.stdinEOF)
	require.NotNil(t, c.stdinDone)
	require.True(t, chanClosed(ioDone), "no stdout/stderr: ioDone must already be closed")
	// Registered first so it runs last, after the client ends opened by the
	// test have been closed: release everything the copy still holds.
	t.Cleanup(func() {
		c.stdinEOF()
		select {
		case <-c.stdinDone:
		case <-time.After(testWait):
			t.Error("stdin copy did not finish during cleanup")
		}
		if c.abort != nil {
			c.abort()
		}
		c.conn.Close()
	})
	return c
}

func (c *stdinCase) waitDone(t *testing.T) {
	t.Helper()
	waitClosed(t, c.stdinDone, "stdinDone")
}

func testPayload(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

// TestStdinDetachReattach: a client closing its own write end is a detach, not
// an end of input. A later client can attach again and its bytes arrive in
// order; the in-band EOF only follows CloseIO.
//
// The proof that no EOF happens in between needs no waiting: the in-band
// CloseWrite is sent by the one goroutine that relays stdin right before it
// exits, so bytes relayed after a detach show it was still running and had
// not sent it.
func TestStdinDetachReattach(t *testing.T) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			c := startStdin(t, impl)

			w1 := openClientEnd(t, c.path, syscall.O_WRONLY)
			_, err := w1.Write([]byte("one"))
			require.NoError(t, err)
			require.NoError(t, w1.Close()) // detach
			c.conn.waitSent(t, []byte("one"))

			w2 := openClientEnd(t, c.path, syscall.O_WRONLY)
			_, err = w2.Write([]byte("two"))
			require.NoError(t, err)
			c.conn.waitSent(t, []byte("onetwo"))
			_, eof, _ := c.conn.snapshot()
			require.False(t, eof, "EOF delivered while the client was attached")
			require.NoError(t, w2.Close()) // detach again

			w3 := openClientEnd(t, c.path, syscall.O_WRONLY)
			_, err = w3.Write([]byte("three"))
			require.NoError(t, err)
			c.conn.waitSent(t, []byte("onetwothree"))
			_, eof, _ = c.conn.snapshot()
			require.False(t, eof, "EOF delivered after a detach, before CloseIO")
			require.False(t, chanClosed(c.stdinDone))
			require.NoError(t, w3.Close())

			require.NoError(t, c.stdinEOF())
			c.waitDone(t)
			sent, eof, late := c.conn.snapshot()
			require.True(t, eof, "CloseIO must deliver the in-band EOF")
			require.False(t, late)
			require.Equal(t, "onetwothree", string(sent))
		})
	}
}

// TestStdinCloseIODeliversBufferedBytesThenEOF: CloseIO after the client wrote
// and closed delivers exactly the bytes that were written, in order, followed
// by the EOF, with nothing relayed after it. The payload is far larger than a
// FIFO buffer so the copy is still draining when CloseIO arrives.
func TestStdinCloseIODeliversBufferedBytesThenEOF(t *testing.T) {
	payload := testPayload(1 << 20)
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			c := startStdin(t, impl)
			w := openClientEnd(t, c.path, syscall.O_WRONLY)
			// Write on this goroutine: it only completes as the copy drains
			// the FIFO, so when it returns most bytes are still in flight.
			n, err := w.Write(payload)
			require.NoError(t, err)
			require.Equal(t, len(payload), n)
			require.NoError(t, w.Close())

			require.NoError(t, c.stdinEOF())
			c.waitDone(t)
			sent, eof, late := c.conn.snapshot()
			require.True(t, eof)
			require.False(t, late, "bytes were relayed after the EOF")
			require.True(t, bytes.Equal(payload, sent), "relayed %d bytes, want %d, in order", len(sent), len(payload))
		})
	}
}

// TestStdinCloseIOBeforeClientCloses: the documented contract is that EOF
// needs every writer gone, including the client's. CloseIO while the client
// still has its end open must not cut off what the client writes afterwards,
// and the EOF must wait for the client to close.
func TestStdinCloseIOBeforeClientCloses(t *testing.T) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			c := startStdin(t, impl)
			w := openClientEnd(t, c.path, syscall.O_WRONLY)

			_, err := w.Write([]byte("before "))
			require.NoError(t, err)
			c.conn.waitSent(t, []byte("before "))

			require.NoError(t, c.stdinEOF())

			// Written after CloseIO, while the client still holds its end.
			_, err = w.Write([]byte("after"))
			require.NoError(t, err)
			c.conn.waitSent(t, []byte("before after"))
			_, eof, _ := c.conn.snapshot()
			require.False(t, eof, "EOF delivered while the client still had its end open")
			require.False(t, chanClosed(c.stdinDone))

			require.NoError(t, w.Close())
			c.waitDone(t)
			sent, eof, late := c.conn.snapshot()
			require.True(t, eof)
			require.False(t, late)
			require.Equal(t, "before after", string(sent))
		})
	}
}

// TestStdinCloseIONoClient: CloseIO with no client ever attached is an
// immediate, empty end of input, and CloseIO can be repeated.
//
// This is run for the implementations that must be able to do it, not for the
// original Linux copyStreams. That one opens the FIFO in the background with
// blocking open(2) calls, and if CloseIO closes its write reference before the
// background open of the read end has paired with anything, that open waits for
// a writer that never comes and the stdin copy never finishes; with GOMAXPROCS=2
// roughly one run in three hangs. Opening without blocking, as the polled
// implementation does, has no such window, so it is repeated here to keep it
// that way.
func TestStdinCloseIONoClient(t *testing.T) {
	for _, impl := range teardownImpls {
		t.Run(impl.name, func(t *testing.T) {
			for range 100 {
				c := startStdin(t, impl)
				require.NoError(t, c.stdinEOF())
				require.NoError(t, c.stdinEOF(), "stdinEOF must be idempotent")
				c.waitDone(t)
				sent, eof, _ := c.conn.snapshot()
				require.True(t, eof)
				require.Empty(t, sent)
				require.NoError(t, c.stdinEOF(), "stdinEOF after completion")
			}
		})
	}
}

// TestStdinClientBlockedInOpen: a client that opened its write end with a
// blocking open(2), as containerd's fifo does in the background, must be paired
// with the shim's reader and its bytes must arrive.
func TestStdinClientBlockedInOpen(t *testing.T) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			path := mkfifo(t, "stdin")
			type opened struct {
				f   *os.File
				err error
			}
			ch := make(chan opened, 1)
			go func() {
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				ch <- opened{f, err}
			}()

			conn := newFakeConn()
			ioDone := make(chan struct{})
			stdinEOF, stdinDone, abort, err := impl.fn(t.Context(), [3]io.ReadWriteCloser{conn, nil, nil}, path, "", "", ioDone)
			require.NoError(t, err)
			t.Cleanup(func() {
				stdinEOF()
				if abort != nil {
					abort()
				}
				conn.Close()
			})

			var o opened
			select {
			case o = <-ch:
			case <-time.After(testWait):
				t.Fatal("the client's blocking open of the stdin FIFO never completed")
			}
			require.NoError(t, o.err)
			_, err = o.f.Write([]byte("hello"))
			require.NoError(t, err)
			conn.waitSent(t, []byte("hello"))
			require.NoError(t, o.f.Close())
			require.NoError(t, stdinEOF())
			waitClosed(t, stdinDone, "stdinDone")
			_, eof, _ := conn.snapshot()
			require.True(t, eof)
		})
	}
}

// TestStdinEOFStress repeats the CloseIO-after-close sequence many times
// concurrently. The last writer closing while the reader waits is where macOS
// can lose a wakeup, so on a Mac this is the real-kernel check that the end of
// input is always found. It cannot prove absence of a rare race, the
// deterministic tests above carry the guarantees.
func TestStdinEOFStress(t *testing.T) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			stressStdinEOF(t, impl, 8, 40)
		})
	}
}

// stressStdinEOF runs rounds stdin copies on each of workers goroutines.
func stressStdinEOF(t *testing.T, impl copyImpl, workers, rounds int) {
	t.Helper()
	var wg sync.WaitGroup
	dir := t.TempDir()
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				if err := stdinRound(t.Context(), impl, filepath.Join(dir, fmt.Sprintf("f-%d-%d", w, r)), w*rounds+r); err != nil {
					t.Errorf("worker %d round %d: %v", w, r, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func stdinRound(ctx context.Context, impl copyImpl, path string, seed int) error {
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return err
	}
	conn := newFakeConn()
	defer conn.Close()
	ioDone := make(chan struct{})
	stdinEOF, stdinDone, abort, err := impl.fn(ctx, [3]io.ReadWriteCloser{conn, nil, nil}, path, "", "", ioDone)
	if err != nil {
		return err
	}
	if abort != nil {
		defer abort()
	}
	payload := testPayload(1 + seed%5000)

	var w *os.File
	for {
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err == nil {
			w = os.NewFile(uintptr(fd), path)
			break
		}
		if !errors.Is(err, syscall.ENXIO) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := w.Write(payload); err != nil {
		w.Close()
		return err
	}
	w.Close()
	if err := stdinEOF(); err != nil {
		return err
	}
	select {
	case <-stdinDone:
	case <-time.After(testWait):
		return errors.New("stdin copy never reached the end of input")
	}
	sent, eof, late := conn.snapshot()
	if !eof || late || !bytes.Equal(sent, payload) {
		return fmt.Errorf("relayed %d bytes (eof=%v late=%v), want %d then eof", len(sent), eof, late, len(payload))
	}
	return nil
}

// TestStdinRegularFile: a stdin that is a plain file is read once to its end,
// then the in-band EOF follows. stdinEOF is a harmless no-op.
func TestStdinRegularFile(t *testing.T) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			payload := testPayload(300_000)
			path := filepath.Join(t.TempDir(), "stdin")
			require.NoError(t, os.WriteFile(path, payload, 0o600))
			conn := newFakeConn()
			stdinEOF, stdinDone, abort, err := impl.fn(t.Context(), [3]io.ReadWriteCloser{conn, nil, nil}, path, "", "", make(chan struct{}))
			require.NoError(t, err)
			if abort != nil {
				defer abort()
			}
			require.NotNil(t, stdinEOF)
			waitClosed(t, stdinDone, "stdinDone")
			sent, eof, late := conn.snapshot()
			require.True(t, bytes.Equal(payload, sent), "relayed stdin bytes differ")
			require.True(t, eof)
			require.False(t, late)
			require.NoError(t, stdinEOF())
		})
	}
}

// failingWriteConn is a stdin stream whose writes to the guest fail.
type failingWriteConn struct{ *fakeConn }

func (failingWriteConn) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// TestStdinCopyFailureIsNotCleanEOF: input that could not be forwarded in full
// must not be followed by the in-band EOF.
func TestStdinCopyFailureIsNotCleanEOF(t *testing.T) {
	for _, kind := range []string{"fifo", "file"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stdin")
			if kind == "fifo" {
				path = mkfifo(t, "stdin")
			} else {
				require.NoError(t, os.WriteFile(path, []byte("input"), 0o600))
			}
			conn := newFakeConn()
			stdinEOF, stdinDone, abort, err := copyStreamsPolled(t.Context(), [3]io.ReadWriteCloser{failingWriteConn{conn}, nil, nil}, path, "", "", make(chan struct{}))
			require.NoError(t, err)
			defer abort()
			if kind == "fifo" {
				w := openClientEnd(t, path, syscall.O_WRONLY)
				_, err := w.Write([]byte("input"))
				require.NoError(t, err)
			}
			waitClosed(t, stdinDone, "stdinDone")
			_, eof, _ := conn.snapshot()
			require.False(t, eof, "a failed stdin copy must not look like a clean EOF")
			require.NoError(t, stdinEOF())
		})
	}
}

// TestOutputCopy: output relayed from the guest reaches the client's FIFOs
// intact and each FIFO sees its end of file when the guest closes the stream.
func TestOutputCopy(t *testing.T) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			stdoutPath, stderrPath := mkfifo(t, "stdout"), mkfifo(t, "stderr")
			outR := openClientEnd(t, stdoutPath, syscall.O_RDONLY)
			errR := openClientEnd(t, stderrPath, syscall.O_RDONLY)
			outC, errC := newFakeConn(), newFakeConn()
			ioDone := make(chan struct{})
			_, _, abort, err := impl.fn(t.Context(), [3]io.ReadWriteCloser{nil, outC, errC}, "", stdoutPath, stderrPath, ioDone)
			require.NoError(t, err)
			t.Cleanup(func() {
				if abort != nil {
					abort()
				}
			})

			outData, errData := testPayload(300_000), testPayload(1234)
			go func() {
				outC.guestOut.Write(outData)
				outC.guestOut.Close()
			}()
			go func() {
				errC.guestOut.Write(errData)
				errC.guestOut.Close()
			}()
			require.True(t, bytes.Equal(outData, readToEOF(t, outR)))
			require.True(t, bytes.Equal(errData, readToEOF(t, errR)))
			waitClosed(t, ioDone, "ioDone")
		})
	}
}

// TestOutputToRegularFiles: stdout and stderr that are plain files, which is
// what the "file" scheme produces, keep working.
func TestOutputToRegularFiles(t *testing.T) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			dir := t.TempDir()
			outPath, errPath := filepath.Join(dir, "out.log"), filepath.Join(dir, "err.log")
			require.NoError(t, os.WriteFile(outPath, nil, 0o600))
			require.NoError(t, os.WriteFile(errPath, nil, 0o600))
			outC, errC := newFakeConn(), newFakeConn()
			ioDone := make(chan struct{})
			_, _, abort, err := impl.fn(t.Context(), [3]io.ReadWriteCloser{nil, outC, errC}, "", outPath, errPath, ioDone)
			require.NoError(t, err)
			if abort != nil {
				defer abort()
			}
			outC.guestOut.Write([]byte("out"))
			outC.guestOut.Close()
			errC.guestOut.Write([]byte("err"))
			errC.guestOut.Close()
			waitClosed(t, ioDone, "ioDone")
			// done is closed before the goroutines close their files; the
			// writes themselves are complete.
			got, err := os.ReadFile(outPath)
			require.NoError(t, err)
			require.Equal(t, "out", string(got))
			got, err = os.ReadFile(errPath)
			require.NoError(t, err)
			require.Equal(t, "err", string(got))
		})
	}
}

// fakeStreams is the streamCreator forwardIO needs: it hands out fakeConns and
// remembers them by the stream they were made for.
type fakeStreams struct {
	mu    sync.Mutex
	conns map[string]*fakeConn
}

func (f *fakeStreams) StartStream(_ context.Context, id string) (net.Conn, error) {
	var kind string
	for _, k := range []string{"stdin", "stdout", "stderr"} {
		if strings.Contains(id, "-"+k+"-") {
			kind = k
		}
	}
	if kind == "" {
		return nil, fmt.Errorf("unexpected stream id %q", id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns == nil {
		f.conns = map[string]*fakeConn{}
	}
	c := newFakeConn()
	f.conns[kind] = c
	return c, nil
}

func (f *fakeStreams) conn(kind string) *fakeConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[kind]
}

// forwarded is a running forwardIOWith.
type forwarded struct {
	streams  *fakeStreams
	shutdown func(context.Context) error
	ioDone   <-chan struct{}
	stdinEOF func() error
}

func forward(t *testing.T, impl copyImpl, sio stdio.Stdio) *forwarded {
	t.Helper()
	f := &forwarded{streams: &fakeStreams{}}
	_, shutdown, ioDone, stdinEOF, err := (&service{}).forwardIOWith(t.Context(), f.streams, "t", sio, impl.fn)
	require.NoError(t, err)
	require.NotNil(t, shutdown)
	f.shutdown, f.ioDone, f.stdinEOF = shutdown, ioDone, stdinEOF
	return f
}

// expiredContext is a context whose deadline has already passed, which makes
// ioShutdown skip its bounded waits as if they had run out.
func expiredContext() (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), time.Unix(1, 0))
}

// TestShutdownReleasesStdinReaderParkedOnClient: the container is gone but the
// client still holds its stdin write end open, so the end of input never comes
// and the stdin copy sits waiting for it. ioShutdown, once its bounded wait
// ran out, must release that reader instead of leaving it behind.
//
// The observable is the FIFO itself: after ioShutdown returns, nothing of the
// shim may hold the read end any more, so the client's next write fails with
// EPIPE. A copy goroutine still parked on a descriptor it never closes would
// keep the FIFO readable and the write would succeed.
func TestShutdownReleasesStdinReaderParkedOnClient(t *testing.T) {
	for _, impl := range teardownImpls {
		t.Run(impl.name, func(t *testing.T) {
			stdinPath, stdoutPath := mkfifo(t, "stdin"), mkfifo(t, "stdout")
			outR := openClientEnd(t, stdoutPath, syscall.O_RDONLY)
			f := forward(t, impl, stdio.Stdio{Stdin: stdinPath, Stdout: stdoutPath})
			stdin := f.streams.conn("stdin")

			w := openClientEnd(t, stdinPath, syscall.O_WRONLY)
			_, err := w.Write([]byte("partial"))
			require.NoError(t, err)
			stdin.waitSent(t, []byte("partial"))

			// The guest process ended and closed its output; the client never
			// closes its stdin.
			f.streams.conn("stdout").guestOut.Close()
			require.Empty(t, readToEOF(t, outR))
			waitClosed(t, f.ioDone, "ioDone")

			ctx, cancel := expiredContext()
			defer cancel()
			err = f.shutdown(ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)

			_, err = w.Write([]byte("x"))
			require.ErrorIs(t, err, syscall.EPIPE, "the shim still has the stdin FIFO open for reading after ioShutdown")

			requireNoCopyGoroutines(t, "a copy goroutine is still running after ioShutdown")
			// Only meaningful once the goroutine is gone: it is what would send it.
			_, eof, _ := stdin.snapshot()
			require.False(t, eof, "an aborted, truncated stdin must not look like a clean EOF")
		})
	}
}

// TestShutdownReleasesOutputWriterBlockedOnClient: the client stopped reading
// the output FIFO, so the copy is blocked writing to it. ioShutdown must be
// able to release it: ioDone only closes once that goroutine has returned.
func TestShutdownReleasesOutputWriterBlockedOnClient(t *testing.T) {
	for _, impl := range teardownImpls {
		t.Run(impl.name, func(t *testing.T) {
			stdoutPath := mkfifo(t, "stdout")
			openClientEnd(t, stdoutPath, syscall.O_RDONLY) // open, never read
			f := forward(t, impl, stdio.Stdio{Stdout: stdoutPath})
			out := f.streams.conn("stdout")

			// An endless supply of output, as from a chatty process.
			go func() {
				chunk := bytes.Repeat([]byte("x"), 4096)
				for {
					if _, err := out.guestOut.Write(chunk); err != nil {
						return
					}
				}
			}()
			// With nobody reading, the FIFO fills up and the copy blocks in
			// write: once a write cannot take a single byte, the shim's own
			// next write cannot complete either.
			fillFifo(t, stdoutPath)
			require.False(t, chanClosed(f.ioDone))

			ctx, cancel := expiredContext()
			defer cancel()
			require.ErrorIs(t, f.shutdown(ctx), context.DeadlineExceeded)
			waitClosed(t, f.ioDone, "ioDone: the output copy is still blocked writing to the client's FIFO")
		})
	}
}

// fillFifo writes into the FIFO until it is full.
func fillFifo(t *testing.T, path string) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer syscall.Close(fd)
	chunk := make([]byte, 512)
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if _, err := syscall.Write(fd, chunk); err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				return
			}
			if !errors.Is(err, syscall.EINTR) {
				t.Fatalf("filling %s: %v", path, err)
			}
		}
	}
	t.Fatalf("%s never filled up", path)
}

// TestShutdownCleanFinish: a process that finished normally tears down
// promptly, without any forced release, and loses nothing.
func TestShutdownCleanFinish(t *testing.T) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) {
			stdinPath, stdoutPath, stderrPath := mkfifo(t, "stdin"), mkfifo(t, "stdout"), mkfifo(t, "stderr")
			outR := openClientEnd(t, stdoutPath, syscall.O_RDONLY)
			errR := openClientEnd(t, stderrPath, syscall.O_RDONLY)
			f := forward(t, impl, stdio.Stdio{Stdin: stdinPath, Stdout: stdoutPath, Stderr: stderrPath})
			stdin := f.streams.conn("stdin")

			w := openClientEnd(t, stdinPath, syscall.O_WRONLY)
			_, err := w.Write([]byte("input"))
			require.NoError(t, err)
			require.NoError(t, w.Close())
			require.NoError(t, f.stdinEOF()) // CloseIO

			f.streams.conn("stdout").guestOut.Write([]byte("output"))
			f.streams.conn("stdout").guestOut.Close()
			f.streams.conn("stderr").guestOut.Write([]byte("errors"))
			f.streams.conn("stderr").guestOut.Close()
			require.Equal(t, "output", string(readToEOF(t, outR)))
			require.Equal(t, "errors", string(readToEOF(t, errR)))
			waitClosed(t, f.ioDone, "ioDone")

			ctx, cancel := context.WithTimeout(context.Background(), testWait)
			defer cancel()
			require.NoError(t, f.shutdown(ctx))

			sent, eof, late := stdin.snapshot()
			require.Equal(t, "input", string(sent))
			require.True(t, eof)
			require.False(t, late)
		})
	}
}

// TestParkedCopiesDoNotPinThreads: a copy goroutine parked in read(2) or
// write(2) on a descriptor that is not on the runtime poller pins an OS thread
// for as long as it is parked, which on macOS is what exhausts the process
// thread limit in a long-lived shim. Many parked stdin readers and many
// blocked output writers must be served by a few threads.
func TestParkedCopiesDoNotPinThreads(t *testing.T) {
	const n = 40
	for _, impl := range teardownImpls {
		t.Run(impl.name, func(t *testing.T) {
			threads := func() int { return pprof.Lookup("threadcreate").Count() }
			before := threads()

			var running []*forwarded
			for range n {
				stdinPath, stdoutPath := mkfifo(t, "stdin"), mkfifo(t, "stdout")
				openClientEnd(t, stdoutPath, syscall.O_RDONLY) // never read
				f := forward(t, impl, stdio.Stdio{Stdin: stdinPath, Stdout: stdoutPath})
				openClientEnd(t, stdinPath, syscall.O_WRONLY) // stays attached
				out := f.streams.conn("stdout")
				go func() {
					chunk := bytes.Repeat([]byte("x"), 4096)
					for {
						if _, err := out.guestOut.Write(chunk); err != nil {
							return
						}
					}
				}()
				fillFifo(t, stdoutPath)
				running = append(running, f)
			}
			// n stdin readers are parked waiting for input and n output writers
			// are blocked on full FIFOs, none of which can finish by itself.
			grew := threads() - before
			require.Less(t, grew, n, "%d parked copies created %d OS threads", 2*n, grew)

			for _, f := range running {
				ctx, cancel := expiredContext()
				f.shutdown(ctx)
				cancel()
			}
			for _, f := range running {
				waitClosed(t, f.ioDone, "ioDone")
			}
			requireNoCopyGoroutines(t, "copy goroutines are still running after ioShutdown")
		})
	}
}

// copyGoroutineStacks returns the stacks of the live goroutines started by
// either copy implementation, so a test sees whether any is left behind.
func copyGoroutineStacks() []string {
	return append(goroutinesIn("task.copyStreams.func"), goroutinesIn("task.copyStreamsPolled.func")...)
}

// requireNoCopyGoroutines waits for every copy goroutine to be gone. When one
// is left it fails with its stack, which says what it is parked on.
func requireNoCopyGoroutines(t *testing.T, msg string) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		left := copyGoroutineStacks()
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s:\n\n%s", msg, strings.Join(left, "\n\n"))
		}
		time.Sleep(time.Millisecond)
	}
}

// goroutinesIn returns the stacks of the goroutines whose stack contains substr.
func goroutinesIn(substr string) []string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var stacks []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, substr) {
			stacks = append(stacks, g)
		}
	}
	return stacks
}
