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

package shimtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/containerd/fifo"
)

// randomSuffix returns a short random hex string derived from /dev/urandom.
func randomSuffix() string {
	b := make([]byte, 4)
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return "0000"
	}
	defer f.Close()
	f.Read(b)
	return fmt.Sprintf("%x", b)
}

// createIOFifos creates stdout and stderr FIFOs in dir.
func createIOFifos(tb testing.TB, dir string) (stdoutPath, stderrPath string) {
	tb.Helper()
	stdoutPath = filepath.Join(dir, "stdout")
	stderrPath = filepath.Join(dir, "stderr")
	if err := syscall.Mkfifo(stdoutPath, 0600); err != nil {
		tb.Fatal("failed to create stdout fifo:", err)
	}
	if err := syscall.Mkfifo(stderrPath, 0600); err != nil {
		tb.Fatal("failed to create stderr fifo:", err)
	}
	return
}

// createStdioFifos creates stdin, stdout, and stderr FIFOs in dir.
func createStdioFifos(tb testing.TB, dir string) (stdinPath, stdoutPath, stderrPath string) {
	tb.Helper()
	stdinPath = filepath.Join(dir, "stdin")
	stdoutPath = filepath.Join(dir, "stdout")
	stderrPath = filepath.Join(dir, "stderr")
	if err := syscall.Mkfifo(stdinPath, 0600); err != nil {
		tb.Fatal("failed to create stdin fifo:", err)
	}
	if err := syscall.Mkfifo(stdoutPath, 0600); err != nil {
		tb.Fatal("failed to create stdout fifo:", err)
	}
	if err := syscall.Mkfifo(stderrPath, 0600); err != nil {
		tb.Fatal("failed to create stderr fifo:", err)
	}
	return
}

// drainFifo opens a FIFO for reading and discards all data in a
// background goroutine. Closes on tb cleanup.
func drainFifo(tb testing.TB, ctx context.Context, path string) {
	tb.Helper()
	f, err := fifo.OpenFifo(ctx, path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		tb.Fatal("failed to open fifo:", err)
	}
	go func() {
		buf := make([]byte, 32768)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
		}
	}()
	tb.Cleanup(func() { f.Close() })
}

// drainFifoInto opens a FIFO for reading and copies data into buf
// (protected by mu).
func drainFifoInto(tb testing.TB, ctx context.Context, path string, buf *bytes.Buffer, mu *sync.Mutex) {
	tb.Helper()
	f, err := fifo.OpenFifo(ctx, path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		tb.Fatal("failed to open fifo:", err)
	}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := f.Read(b)
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
	tb.Cleanup(func() { f.Close() })
}

// drainFifoIntoDone opens a FIFO for reading, copies all data into buf
// (protected by mu), and closes a done channel when the reader goroutine
// exits (i.e. when the write end is closed). Use this instead of
// drainFifoInto when the caller needs to block until the FIFO is fully
// drained before inspecting buf.
func drainFifoIntoDone(tb testing.TB, ctx context.Context, path string, buf *bytes.Buffer, mu *sync.Mutex) <-chan struct{} {
	tb.Helper()
	f, err := fifo.OpenFifo(ctx, path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		tb.Fatal("failed to open fifo:", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 4096)
		for {
			n, err := f.Read(b)
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
	tb.Cleanup(func() { f.Close() })
	return done
}

// openPipeWriter opens the write end of a FIFO at path. Used by tests
// that write to a container's stdin.
func openPipeWriter(ctx context.Context, path string) (io.WriteCloser, error) {
	return fifo.OpenFifo(ctx, path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
}

// openPipeReader opens the read end of a FIFO at path for direct
// synchronous reads (benchmarks, round-trip tests).
func openPipeReader(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	return fifo.OpenFifo(ctx, path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// createRawPipeWriter creates a FIFO and returns its path, a WriteCloser
// for the write end (host → shim direction), and a cleanup function. Used
// in contexts without a testing.TB (e.g. runExecRoundTrip in stress_suite.go).
func createRawPipeWriter(dir, name string) (path string, w io.WriteCloser, cleanup func(), err error) {
	path = filepath.Join(dir, name)
	if err = syscall.Mkfifo(path, 0600); err != nil {
		return "", nil, nil, fmt.Errorf("mkfifo %s: %w", path, err)
	}
	f, err := fifo.OpenFifo(context.Background(), path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, nil, fmt.Errorf("open fifo %s: %w", path, err)
	}
	return path, f, func() { f.Close() }, nil
}

// createRawPipe creates a FIFO and returns its path, a ReadCloser for
// the read end, and a cleanup function. Used in contexts without a
// testing.TB (e.g. runOneExec in stress_suite.go).
func createRawPipe(dir, name string) (path string, r io.ReadCloser, cleanup func(), err error) {
	path = filepath.Join(dir, name)
	if err = syscall.Mkfifo(path, 0600); err != nil {
		return "", nil, nil, fmt.Errorf("mkfifo %s: %w", path, err)
	}
	f, err := fifo.OpenFifo(context.Background(), path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, nil, fmt.Errorf("open fifo %s: %w", path, err)
	}
	return path, f, func() { f.Close() }, nil
}

// setupLogPipe creates a log FIFO at bundleDir/log and returns a
// ReadCloser. The ns and id parameters are unused on Unix (the shim
// writes to bundleDir/log by convention).
func setupLogPipe(tb testing.TB, bundleDir, ns, id string) io.ReadCloser {
	tb.Helper()
	logPath := filepath.Join(bundleDir, "log")
	if err := syscall.Mkfifo(logPath, 0700); err != nil {
		tb.Fatal("failed to create log fifo:", err)
	}
	f, err := fifo.OpenFifo(context.Background(), logPath, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		tb.Fatal("failed to open log fifo:", err)
	}
	return f
}
