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
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// makeFifo creates a FIFO in a fresh temp dir and returns its path.
func makeFifo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return path
}

// openReaderRaw opens the read end of a FIFO without blocking. It touches no
// *testing.T, so it is safe to call from a goroutine. Holding a reader open
// lets the write-end open succeed.
func openReaderRaw(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openReader opens the read end of a FIFO and registers it for cleanup. Must be
// called from the test goroutine.
func openReader(t *testing.T, path string) *os.File {
	t.Helper()
	r, err := openReaderRaw(path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// TestOpenFifoWriteRendezvousReaderPresent: with a reader already open, the
// write end opens immediately, has O_NONBLOCK cleared (so writes block for
// backpressure), and bytes flow through to the reader.
func TestOpenFifoWriteRendezvousReaderPresent(t *testing.T) {
	path := makeFifo(t)
	r := openReader(t, path)

	fw, err := openFifoWriteRendezvous(t.Context(), path)
	if err != nil {
		t.Fatalf("rendezvous with reader present: %v", err)
	}
	defer fw.Close()

	fl, err := unix.FcntlInt(fw.Fd(), syscall.F_GETFL, 0)
	if err != nil {
		t.Fatalf("F_GETFL: %v", err)
	}
	if fl&syscall.O_NONBLOCK != 0 {
		t.Fatal("expected O_NONBLOCK to be cleared on the write fd")
	}

	if _, err := fw.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if buf[0] != 'x' {
		t.Fatalf("got %q, want \"x\"", buf)
	}
}

// TestOpenFifoWriteRendezvousReaderArrivesLate: the open must not stall at
// start; it polls past ENXIO and succeeds once a reader shows up.
func TestOpenFifoWriteRendezvousReaderArrivesLate(t *testing.T) {
	path := makeFifo(t)

	type readerResult struct {
		r   *os.File
		err error
	}
	// Buffered so the goroutine never blocks on send, and never touches t.
	ready := make(chan readerResult, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		r, err := openReaderRaw(path)
		ready <- readerResult{r: r, err: err}
	}()

	start := time.Now()
	fw, err := openFifoWriteRendezvous(t.Context(), path)
	if err != nil {
		t.Fatalf("rendezvous with late reader: %v", err)
	}
	defer fw.Close()

	res := <-ready
	if res.err != nil {
		t.Fatalf("open reader: %v", res.err)
	}
	t.Cleanup(func() { res.r.Close() })

	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("rendezvous returned in %s; expected it to wait for the reader", elapsed)
	}
}

// TestOpenFifoWriteRendezvousNoReaderTimesOut: with no reader ever opening,
// the open fails fast once the context deadline elapses rather than blocking
// forever.
func TestOpenFifoWriteRendezvousNoReaderTimesOut(t *testing.T) {
	path := makeFifo(t)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	fw, err := openFifoWriteRendezvous(ctx, path)
	if err == nil {
		fw.Close()
		t.Fatal("expected an error with no reader present, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("rendezvous took %s to give up; expected it to honor the context deadline", elapsed)
	}
}

// TestOpenFifoWriteRendezvousRejectsSymlink: O_NOFOLLOW must reject a symlinked
// final path component (a client-supplied path swapped for a symlink after the
// daemon validated it), even with a reader present.
func TestOpenFifoWriteRendezvousRejectsSymlink(t *testing.T) {
	path := makeFifo(t)
	openReader(t, path)

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	fw, err := openFifoWriteRendezvous(t.Context(), link)
	if err == nil {
		fw.Close()
		t.Fatal("expected O_NOFOLLOW to reject the symlinked path, got nil")
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("got %v, want ELOOP", err)
	}
}
