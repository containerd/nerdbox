//go:build windows

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

package shimtest

// Windows IO layer for shimtest.
//
// On Windows, containerd uses named pipes for container stdio and for the shim
// log stream, following the same roles as containerd's own pkg/cio/io_windows.go
// and core/runtime/v2/shim_windows.go:
//
//   Stdio (stdout / stderr / stdin):
//     The test harness is the named-pipe SERVER; the shim is the CLIENT.
//     We call winio.ListenPipe before handing the path to the shim so that
//     the pipe exists when the shim tries to connect.
//
//   Log pipe:
//     The shim is the SERVER (creates \\.\pipe\containerd-shim-<ns>-<id>-log).
//     The test harness is the CLIENT and dials with a short retry loop after
//     the shim's "start" sub-command returns.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// pipeCounter ensures each pipe name is unique within the process even when
// pipeName() is called multiple times within the same nanosecond.
var pipeCounter uint64

// randomSuffix returns a unique hex string combining time, pid, and a
// monotonically increasing counter.
func randomSuffix() string {
	n := atomic.AddUint64(&pipeCounter, 1)
	v := uint32(time.Now().UnixNano()) ^ uint32(os.Getpid())
	return fmt.Sprintf("%08x%04x", v, n)
}

// pipeName returns a unique Windows named-pipe path.
func pipeName() string {
	return `\\.\pipe\shimtest-` + randomSuffix()
}

// pipeListeners holds the server-side net.Listener for each named pipe
// created by createIOFifos / createStdioFifos. drainFifo / drainFifoInto /
// openPipeWriter / openPipeReader consume entries from this map.
var pipeListeners sync.Map // map[string]net.Listener

// pipeCfg is the pipe configuration used for all shimtest named pipes.
// nil uses the default named pipe ACL (current user has full access),
// which is what containerd itself passes to winio.ListenPipe.
var pipeCfg *winio.PipeConfig

// listenPipe creates a named-pipe server at path, stores the listener in
// pipeListeners, and registers a cleanup on tb.
func listenPipe(tb testing.TB, path string) {
	tb.Helper()
	l, err := winio.ListenPipe(path, pipeCfg)
	if err != nil {
		tb.Fatalf("ListenPipe %s: %v", path, err)
	}
	pipeListeners.Store(path, l)
	tb.Cleanup(func() {
		l.Close()
		pipeListeners.Delete(path)
	})
}

// popListener retrieves the listener for path from pipeListeners.
// The caller is responsible for closing it when done.
func popListener(tb testing.TB, path string) net.Listener {
	tb.Helper()
	v, ok := pipeListeners.Load(path)
	if !ok {
		tb.Fatalf("no named-pipe listener registered for %s", path)
	}
	return v.(net.Listener)
}

// createIOFifos creates Windows named-pipe paths for stdout and stderr,
// starts server-side listeners, and returns the paths.
func createIOFifos(tb testing.TB, _ string) (stdoutPath, stderrPath string) {
	tb.Helper()
	stdoutPath = pipeName()
	stderrPath = pipeName()
	listenPipe(tb, stdoutPath)
	listenPipe(tb, stderrPath)
	return
}

// createStdioFifos creates Windows named-pipe paths for stdin, stdout, and
// stderr and starts listeners for all three.
func createStdioFifos(tb testing.TB, _ string) (stdinPath, stdoutPath, stderrPath string) {
	tb.Helper()
	stdinPath = pipeName()
	stdoutPath = pipeName()
	stderrPath = pipeName()
	listenPipe(tb, stdinPath)
	listenPipe(tb, stdoutPath)
	listenPipe(tb, stderrPath)
	return
}

// drainFifo accepts one connection on the named pipe at path and discards all
// data in a background goroutine. Mirrors containerd's stdout/stderr handling
// in pkg/cio/io_windows.go.
func drainFifo(tb testing.TB, _ context.Context, path string) {
	tb.Helper()
	l := popListener(tb, path)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 32768)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()
}

// drainFifoInto accepts one connection on the named pipe at path and copies
// data into buf (protected by mu) in a background goroutine.
func drainFifoInto(tb testing.TB, _ context.Context, path string, buf *bytes.Buffer, mu *sync.Mutex) {
	tb.Helper()
	l := popListener(tb, path)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b := make([]byte, 4096)
		for {
			n, err := c.Read(b)
			if n > 0 {
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
}

// openPipeWriter accepts one connection on the named pipe at path and returns
// a WriteCloser. Used by tests that write to a container's stdin.
//
// The Accept is done in a background goroutine; writes to the returned writer
// block until the shim connects (matching the blocking behaviour of
// fifo.OpenFifo with O_WRONLY on Linux).
func openPipeWriter(ctx context.Context, path string) (io.WriteCloser, error) {
	v, ok := pipeListeners.Load(path)
	if !ok {
		return nil, fmt.Errorf("no named-pipe listener registered for %s", path)
	}
	l := v.(net.Listener)

	pr, pw := io.Pipe()
	go func() {
		c, err := l.Accept()
		if err != nil {
			pr.CloseWithError(err)
			return
		}
		defer c.Close()
		io.Copy(c, pr)
	}()
	return pw, nil
}

// openPipeReader accepts one connection on the named pipe at path and returns
// a ReadWriteCloser. Used in benchmarks and round-trip tests that need direct
// synchronous reads from a container's stdout.
//
// Reads on the returned value block until the shim connects, mirroring the
// deferred-connection pattern in core/runtime/v2/shim_windows.go.
func openPipeReader(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	v, ok := pipeListeners.Load(path)
	if !ok {
		return nil, fmt.Errorf("no named-pipe listener registered for %s", path)
	}
	l := v.(net.Listener)

	// Bridge the Accept goroutine to the caller via io.Pipe so that Read
	// blocks gracefully until the shim connects.
	pr, pw := io.Pipe()
	go func() {
		c, err := l.Accept()
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		defer c.Close()
		io.Copy(pw, c)
		pw.Close()
	}()
	return &pipeReaderRWC{pr}, nil
}

// pipeReaderRWC wraps io.PipeReader to satisfy io.ReadWriteCloser.
// Write always returns an error; callers only Read from this end.
type pipeReaderRWC struct{ *io.PipeReader }

func (p *pipeReaderRWC) Write([]byte) (int, error) {
	return 0, fmt.Errorf("pipeReaderRWC: write not supported")
}

// rawPipeListener is the non-TB equivalent of listenPipe, used by
// createRawPipe for contexts that have no testing.TB (e.g. runOneExec).
type rawPipeState struct {
	l  net.Listener
	pr *io.PipeReader
	pw *io.PipeWriter
}

// createRawPipe creates a named-pipe server and returns its path, a
// ReadCloser for the read end, and a cleanup function. Used in contexts
// without a testing.TB (e.g. runOneExec in stress_suite.go).
func createRawPipe(_, name string) (path string, r io.ReadCloser, cleanup func(), err error) {
	path = `\\.\pipe\shimtest-raw-` + name + `-` + randomSuffix()
	l, err := winio.ListenPipe(path, pipeCfg)
	if err != nil {
		return "", nil, nil, fmt.Errorf("ListenPipe %s: %w", path, err)
	}
	pr, pw := io.Pipe()
	go func() {
		c, err := l.Accept()
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		defer c.Close()
		io.Copy(pw, c)
		pw.Close()
	}()
	cleanup = func() {
		l.Close()
		pr.Close()
	}
	return path, pr, cleanup, nil
}

// setupLogPipe connects to the shim's log named pipe as a client.
//
// On Windows the shim is the SERVER: it creates
//
//	\\.\pipe\containerd-shim-<ns>-<id>-log
//
// and containerd (or our test harness) dials it as the client, exactly as
// containerd does in core/runtime/v2/shim_windows.go openShimLog.
//
// We use a deferredPipeReader so that the dial goroutine starts immediately
// but Read calls block until the connection is established, allowing the
// log goroutine in startShim to start before the shim process exits.
func setupLogPipe(_ testing.TB, _, ns, id string) io.ReadCloser {
	pipePath := fmt.Sprintf(`\\.\pipe\containerd-shim-%s-%s-log`, ns, id)
	return &deferredPipeReader{
		ch: dialPipeAsync(pipePath, 10*time.Second),
	}
}

// dialPipeAsync dials a Windows named pipe in a goroutine, retrying for up to
// timeout until the pipe is created. Returns nil conn on timeout or error.
//
// Mirrors containerd's AnonDialer in pkg/shim/util_windows.go: the pipe may not
// exist yet when we start dialing (the shim creates it asynchronously), so we
// retry every 10ms until either the dial succeeds or we hit the timeout.
func dialPipeAsync(path string, timeout time.Duration) <-chan net.Conn {
	ch := make(chan net.Conn, 1)
	go func() {
		deadline := time.Now().Add(timeout)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			c, err := winio.DialPipeContext(ctx, path)
			cancel()
			if err == nil {
				ch <- c
				return
			}
			if time.Now().After(deadline) {
				ch <- nil
				return
			}
			// Pipe not yet created or transient error: keep trying.
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ch
}

// deferredPipeReader implements io.ReadCloser. Read blocks until the
// background dial completes, then proxies to the connection. This matches
// the deferredPipeConnection pattern in containerd's shim_windows.go.
type deferredPipeReader struct {
	ch   <-chan net.Conn
	conn net.Conn   // set on first Read after dial completes
	once sync.Once  // ensures we receive from ch exactly once
	err  error
}

func (d *deferredPipeReader) wait() {
	d.once.Do(func() {
		c := <-d.ch
		if c == nil {
			d.err = fmt.Errorf("failed to connect to shim log pipe")
			return
		}
		d.conn = c
	})
}

func (d *deferredPipeReader) Read(p []byte) (int, error) {
	d.wait()
	if d.err != nil {
		return 0, d.err
	}
	return d.conn.Read(p)
}

func (d *deferredPipeReader) Close() error {
	d.wait()
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}
