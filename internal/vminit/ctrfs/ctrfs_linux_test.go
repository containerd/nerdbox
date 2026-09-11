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
	"os"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

// These tests cover the registry's bookkeeping: which container a reference is
// held for, and when it is dropped. They deliberately do not cover what a
// container's filesystem looks like from inside its mount namespace.
//
// Joining a mount namespace requires CAP_SYS_ADMIN in the namespace's user
// namespace, so Do cannot reach fn here, and a test that built a stand-in
// namespace would be asserting the kernel's mount resolution rather than
// anything this package decides. Resolution is covered end to end, against a
// real container, by the shim conformance suite in test/shim.
//
// Pinning itself needs no privileges: any process may open its own
// /proc/<pid>/ns/mnt. The tests below pin the test process for that reason.

// selfPid is a pid that is guaranteed to exist for the duration of a test, so
// Add has something real to pin without a container being involved.
func selfPid() int { return os.Getpid() }

func TestAddRejectsInvalidArguments(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	if err := r.Add("", selfPid()); !errdefs.IsInvalidArgument(err) {
		t.Errorf("Add with an empty ID: err = %v, want InvalidArgument", err)
	}
	if err := r.Add("ctr", 0); !errdefs.IsInvalidArgument(err) {
		t.Errorf("Add with a zero pid: err = %v, want InvalidArgument", err)
	}
	if err := r.Add("ctr", -1); !errdefs.IsInvalidArgument(err) {
		t.Errorf("Add with a negative pid: err = %v, want InvalidArgument", err)
	}
}

// TestAddRejectsUnknownPid covers a container whose process is already gone by
// the time its namespace would be pinned: there is nothing to reference, and
// the failure has to be reported rather than recorded as a usable entry.
func TestAddRejectsUnknownPid(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	// Above the maximum pid on any current kernel, so nothing can occupy it.
	const absentPid = 1 << 30

	if err := r.Add("ctr", absentPid); err == nil {
		t.Fatal("Add succeeded for a pid that does not exist")
	}
	if err := r.Do("ctr", func() error { return nil }); !errdefs.IsNotFound(err) {
		t.Errorf("Do after a failed Add: err = %v, want NotFound", err)
	}
}

// TestDoUnknownContainer covers a transfer naming a container that was never
// recorded. It must be reported rather than resolved against whatever
// filesystem the caller happens to be looking at.
func TestDoUnknownContainer(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	called := false
	err := r.Do("absent", func() error {
		called = true
		return nil
	})
	if !errdefs.IsNotFound(err) {
		t.Errorf("Do for an unknown container: err = %v, want NotFound", err)
	}
	if called {
		t.Error("Do ran the operation for an unknown container")
	}
}

// TestDoAfterRelease checks that releasing a container withdraws access rather
// than leaving a usable entry behind.
func TestDoAfterRelease(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	if err := r.Add("ctr", selfPid()); err != nil {
		t.Fatal(err)
	}
	if err := r.Release("ctr"); err != nil {
		t.Fatal(err)
	}

	if err := r.Do("ctr", func() error { return nil }); !errdefs.IsNotFound(err) {
		t.Errorf("Do after Release: err = %v, want NotFound", err)
	}
}

// TestReleaseIsIdempotent covers cleanup paths that may run more than once, and
// containers that were never recorded because their pin failed.
func TestReleaseIsIdempotent(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	if err := r.Release("absent"); err != nil {
		t.Errorf("Release of an unknown container: %v", err)
	}

	if err := r.Add("ctr", selfPid()); err != nil {
		t.Fatal(err)
	}
	if err := r.Release("ctr"); err != nil {
		t.Errorf("first Release: %v", err)
	}
	if err := r.Release("ctr"); err != nil {
		t.Errorf("second Release: %v", err)
	}
}

// TestAddReplacesExistingEntry covers a container ID being recorded twice,
// which happens when an ID is reused. The later reference must win, so a
// recreated container is not resolved through its predecessor's namespace.
func TestAddReplacesExistingEntry(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	if err := r.Add("ctr", selfPid()); err != nil {
		t.Fatal(err)
	}
	first := handleFor(t, r, "ctr")

	if err := r.Add("ctr", selfPid()); err != nil {
		t.Fatal(err)
	}
	second := handleFor(t, r, "ctr")

	if first == second {
		t.Fatal("Add reused the previous reference instead of replacing it")
	}
	// The replaced reference must be closed, otherwise re-creating containers
	// would accumulate namespaces that the kernel can never tear down.
	if !first.isClosed() {
		t.Error("Add left the replaced reference open")
	}
	if second.isClosed() {
		t.Error("Add closed the reference it recorded")
	}
}

// TestCloseReleasesEverything covers registry shutdown: every reference is
// dropped, so nothing keeps a namespace alive past the registry's lifetime.
func TestCloseReleasesEverything(t *testing.T) {
	r := NewRegistry()

	for _, id := range []string{"one", "two", "three"} {
		if err := r.Add(id, selfPid()); err != nil {
			t.Fatal(err)
		}
	}
	handles := map[string]*Handle{}
	for _, id := range []string{"one", "two", "three"} {
		handles[id] = handleFor(t, r, id)
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	for id, h := range handles {
		if !h.isClosed() {
			t.Errorf("Close left the reference for %s open", id)
		}
		if err := r.Do(id, func() error { return nil }); !errdefs.IsNotFound(err) {
			t.Errorf("Do for %s after Close: err = %v, want NotFound", id, err)
		}
	}
}

// handleFor returns the reference the registry holds for id.
func handleFor(t *testing.T, r *Registry, id string) *Handle {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handles[id]
	if !ok {
		t.Fatalf("no reference recorded for %s", id)
	}
	return h
}

func (h *Handle) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// TestCloseDoesNotWaitForOperations is the property that keeps releasing a
// container cheap.
//
// An operation holds its reference for as long as it runs, which for a
// transfer is as long as the copy takes. Callers release containers while
// holding locks the rest of the service needs, so a Close that waited for
// the copy would hold those locks for the same duration.
func TestCloseDoesNotWaitForOperations(t *testing.T) {
	h, err := pin(selfPid())
	if err != nil {
		t.Fatal(err)
	}

	// Stand in for an operation that has started and not yet finished.
	fd, err := h.acquire()
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked while an operation held a reference")
	}

	// The operation still has a usable descriptor: closing it here would
	// leave setns either failing or, once the number was reused, joining an
	// unrelated namespace.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		t.Errorf("the descriptor was closed while still in use: %v", err)
	}

	// No further operation may start, even though the descriptor is open.
	if _, err := h.acquire(); !errdefs.IsUnavailable(err) {
		t.Errorf("acquire after Close: err = %v, want Unavailable", err)
	}

	// Finishing the operation is what finally closes it.
	h.release()
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
		t.Error("the descriptor is still open after the last reference was released")
	}
}

// TestAddDropsPreviousEntryWhenPinFails covers a container ID being reused
// when the new container can no longer be pinned.
//
// The caller records the new container whether or not the pin succeeded, and
// the delete that follows only cleans up what it recognises. A reference left
// over from the previous container would then answer for the new one, so
// operations would reach the predecessor's filesystem instead of failing.
func TestAddDropsPreviousEntryWhenPinFails(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	if err := r.Add("ctr", selfPid()); err != nil {
		t.Fatal(err)
	}
	first := handleFor(t, r, "ctr")

	// Above the maximum pid on any current kernel, so the pin cannot succeed.
	const absentPid = 1 << 30
	if err := r.Add("ctr", absentPid); err == nil {
		t.Fatal("Add succeeded for a pid that does not exist")
	}

	if err := r.Do("ctr", func() error { return nil }); !errdefs.IsNotFound(err) {
		t.Errorf("Do after a failed re-Add: err = %v, want NotFound", err)
	}
	if !first.isClosed() {
		t.Error("the previous reference is still open")
	}
}
