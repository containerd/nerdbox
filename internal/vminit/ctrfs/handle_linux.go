//go:build linux

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

package ctrfs

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

// Handle is a pinned reference to a container's mount namespace.
//
// Holding a Handle keeps the namespace and its entire mount tree alive
// independently of whether any process in it is still running.
type Handle struct {
	mu sync.Mutex
	fd int
	// closed records that the registry has given up its reference, so no
	// further operation may start on this handle.
	closed bool
	// refs counts the operations currently using fd, plus one for the
	// registry's own reference until Close drops it. The descriptor is
	// closed when the count reaches zero.
	//
	// Counting references rather than excluding Close for the duration of
	// an operation is what keeps Close from waiting on one. An operation
	// holds the namespace open for as long as it runs, which for a
	// transfer is as long as the copy takes, and callers release handles
	// while holding locks that the rest of the service needs. The
	// descriptor still cannot be closed while setns is using it, which is
	// what matters: closing underneath setns would at best fail with EBADF
	// and at worst, once the descriptor number was reused, join an
	// unrelated namespace.
	refs int
}

// pin takes a reference to the mount namespace of pid that outlives the
// process. The namespace is destroyed only once every reference is dropped, so
// the open descriptor alone keeps the mount tree intact after pid exits.
func pin(pid int) (*Handle, error) {
	nsPath := fmt.Sprintf("/proc/%d/ns/mnt", pid)
	fd, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", nsPath, err)
	}
	return &Handle{fd: fd, refs: 1}, nil
}

// acquire takes a reference for an operation about to use the namespace and
// returns the descriptor to use. The descriptor stays valid until the
// matching release, whether or not Close is called in the meantime.
func (h *Handle) acquire() (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return -1, errdefs.ErrUnavailable.WithMessage("mount namespace handle is closed")
	}
	h.refs++
	return h.fd, nil
}

// release drops a reference taken by acquire, closing the descriptor if the
// registry has already given up its own and this was the last one.
//
// A failure to close is dropped rather than returned: the operation that took
// the reference has no interest in it, and the descriptor is unusable either
// way.
func (h *Handle) release() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.refs--
	if h.refs == 0 {
		_ = unix.Close(h.fd)
	}
}

// Close drops the registry's reference to the namespace. Once no reference
// remains the kernel tears down the namespace's mount tree and releases the
// filesystems underneath it. Close is idempotent.
//
// Close does not wait for operations already running against the handle. It
// refuses any that have not started yet and leaves the last one still running
// to close the descriptor, so releasing a container never blocks for as long
// as a transfer takes.
func (h *Handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return nil
	}
	h.closed = true
	h.refs--
	if h.refs > 0 {
		return nil
	}
	return unix.Close(h.fd)
}

// Do runs fn on a thread that has joined the container's mount namespace, so
// paths inside fn resolve as they do for the container's own processes:
// through its mounts, in its stacking order, and anchored at its root. fn must
// do all of its filesystem work on the calling goroutine, because only that
// thread has the container's view and anything handed to another goroutine
// resolves against vminitd's filesystem instead, silently rather than failing.
// fn should also be brief, since it occupies a thread that cannot be reused.
func (h *Handle) Do(fn func() error) error {
	fd, err := h.acquire()
	if err != nil {
		return err
	}
	defer h.release()

	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Never unlocked. Joining is irreversible for this thread, and the
		// runtime terminates a locked thread once its goroutine returns, so
		// returning is what retires the thread rather than handing it back
		// carrying the container's filesystem view.

		// setns(CLONE_NEWNS) is refused unless the thread's fs_struct, which
		// holds its root and working directory, is its own. Go clones every
		// thread with CLONE_FS, so it has to be unshared first.
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			errCh <- fmt.Errorf("failed to unshare fs_struct: %w", err)
			return
		}
		if err := unix.Setns(fd, unix.CLONE_NEWNS); err != nil {
			errCh <- fmt.Errorf("failed to join mount namespace: %w", err)
			return
		}
		errCh <- fn()
	}()

	return <-errCh
}
