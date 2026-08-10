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

package sharedresources

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// requireNamespacePrivileges skips a test unless it can actually create and
// bind-mount namespaces. Creating them needs CAP_SYS_ADMIN, and the paths are
// absolute (/run/...), so these only run as real root.
func requireNamespacePrivileges(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("requires root to unshare and bind-mount namespaces")
	}
}

// useTestAnchor points the PID namespace anchor at a host binary that blocks
// forever, standing in for the real anchor (which only ships inside the guest
// rootfs). Any process that does not exit will do: what matters to the
// namespace is that its PID 1 stays alive.
func useTestAnchor(t *testing.T) {
	t.Helper()
	const sleep = "/bin/sleep"
	if _, err := os.Stat(sleep); err != nil {
		t.Skipf("no stand-in anchor binary available: %v", err)
	}
	saved := anchorCommand
	anchorCommand = []string{sleep, "infinity"}
	t.Cleanup(func() { anchorCommand = saved })
}

// isMountPoint reports whether path is a mount point, by comparing its device
// with that of the directory containing it. A pinned namespace is an nsfs
// mount, so once mounted its device always differs from the tmpfs directory it
// sits in.
func isMountPoint(t *testing.T, path string) bool {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return false
	}
	var dir unix.Stat_t
	if err := unix.Lstat(filepath.Dir(path), &dir); err != nil {
		t.Fatalf("lstat %s: %v", filepath.Dir(path), err)
	}
	return st.Dev != dir.Dev
}

// TestManagerCreateDeleteRoundTrip exercises real namespace creation and
// deletion for every supported type: each must end up bind-mounted at the
// returned path, be returned again unchanged on a repeat request, and be fully
// gone after Delete.
//
// The PID namespace's anchor is substituted for a host stand-in, since the
// real one ships only in the guest rootfs; the anchor's liveness is asserted
// too, because that is what keeps the namespace usable.
func TestManagerCreateDeleteRoundTrip(t *testing.T) {
	requireNamespacePrivileges(t)
	useTestAnchor(t)

	ctx := context.Background()
	id := "nerdbox-test-" + randomSuffix(t)
	types := []Type{TypeNamespaceNetwork, TypeNamespaceIPC, TypeNamespacePID, TypeDevShm}
	const devShmSize = 4 * 1024 * 1024

	var m Manager
	t.Cleanup(func() {
		// Best effort, in case an assertion below fails before Delete runs.
		_ = m.Delete(ctx, id, nil)
	})

	paths, err := m.Create(ctx, id, types, devShmSize)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(paths) != len(types) {
		t.Fatalf("Create returned %d paths, want %d", len(paths), len(types))
	}
	for _, typ := range types {
		path, ok := paths[typ]
		if !ok {
			t.Fatalf("Create returned no path for %s", typ)
		}
		if !isMountPoint(t, path) {
			t.Errorf("%s namespace at %s is not a mount point", typ, path)
		}
	}

	// A repeat request must reuse what already exists rather than creating
	// anything new, and must report the same paths. A different size is
	// passed here specifically to confirm it is ignored for an
	// already-created devshm resource, per Create's doc comment.
	again, err := m.Create(ctx, id, types, devShmSize*2)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	for _, typ := range types {
		if again[typ] != paths[typ] {
			t.Errorf("%s path changed across calls: %q then %q", typ, paths[typ], again[typ])
		}
	}

	// Requesting a subset must not disturb the rest.
	subset, err := m.Create(ctx, id, []Type{TypeNamespaceIPC}, 0)
	if err != nil {
		t.Fatalf("subset Create: %v", err)
	}
	if len(subset) != 1 || subset[TypeNamespaceIPC] != paths[TypeNamespaceIPC] {
		t.Errorf("subset Create = %v, want just the IPC path %q", subset, paths[TypeNamespaceIPC])
	}

	// The PID namespace is only usable for as long as its anchor lives, so the
	// anchor must still be running at this point.
	anchor := anchorPID(t, &m, id)
	if !processAlive(anchor) {
		t.Errorf("PID namespace anchor (pid %d) is not running", anchor)
	}

	if err := m.Delete(ctx, id, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, typ := range types {
		path := paths[typ]
		if isMountPoint(t, path) {
			t.Errorf("%s namespace at %s is still mounted after Delete", typ, path)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s bind-mount target %s still exists after Delete (err=%v)", typ, path, err)
		}
	}

	// Deleting the PID namespace means killing its anchor, otherwise the
	// process would outlive the sandbox.
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(anchor) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(anchor) {
		t.Errorf("PID namespace anchor (pid %d) still running after Delete", anchor)
	}

	// Delete must be idempotent.
	if err := m.Delete(ctx, id, nil); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// anchorPID returns the pid of the anchor process holding the group's PID
// namespace open.
func anchorPID(t *testing.T, m *Manager, id string) int {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.ns[key{id: id, typ: TypeNamespacePID}]
	if !ok || e.anchor == nil {
		t.Fatalf("no PID namespace anchor recorded for %q", id)
	}
	return e.anchor.Pid
}

// processAlive reports whether pid is still running. The anchor is a child of
// this process, so once it has been killed and reaped the signal fails.
func processAlive(pid int) bool {
	return unix.Kill(pid, 0) == nil
}

// TestManagerCreateOnlyRequestedTypes verifies that asking for one type does
// not create the others. This is the guard for the PID namespace in
// particular, whose creation costs a persistent anchor process.
func TestManagerCreateOnlyRequestedTypes(t *testing.T) {
	requireNamespacePrivileges(t)

	ctx := context.Background()
	id := "nerdbox-test-" + randomSuffix(t)

	var m Manager
	t.Cleanup(func() { _ = m.Delete(ctx, id, nil) })

	if _, err := m.Create(ctx, id, []Type{TypeNamespaceIPC}, 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, typ := range []Type{TypeNamespaceNetwork, TypeNamespacePID, TypeDevShm} {
		dir, err := typ.dir()
		if err != nil {
			t.Fatal(err)
		}
		path := dir + "/" + id
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s namespace at %s was created without being requested (err=%v)", typ, path, err)
		}
	}
}

// TestManagerDevShmSizeEnforced verifies that the devshm resource is a real,
// size-limited tmpfs — not just a directory — by writing past the requested
// size and confirming the kernel itself rejects it with ENOSPC.
func TestManagerDevShmSizeEnforced(t *testing.T) {
	requireNamespacePrivileges(t)

	ctx := context.Background()
	id := "nerdbox-test-" + randomSuffix(t)
	const size = 1 * 1024 * 1024 // 1MiB

	var m Manager
	t.Cleanup(func() { _ = m.Delete(ctx, id, nil) })

	paths, err := m.Create(ctx, id, []Type{TypeDevShm}, size)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	path := paths[TypeDevShm]

	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		t.Fatalf("statfs %s: %v", path, err)
	}
	gotSize := int64(st.Blocks) * st.Bsize //nolint:unconvert // Bsize is int64 on some arches, int32 on others
	if gotSize != size {
		t.Errorf("tmpfs total size = %d bytes, want %d", gotSize, size)
	}

	f, err := os.Create(filepath.Join(path, "toobig"))
	if err != nil {
		t.Fatalf("create file in devshm: %v", err)
	}
	defer f.Close()

	// Writing past the tmpfs's size must fail with ENOSPC. A plain
	// directory (no size limit at all) would happily accept this.
	buf := make([]byte, size*2)
	_, err = f.Write(buf)
	if !errors.Is(err, unix.ENOSPC) {
		t.Errorf("write past tmpfs size = %v, want ENOSPC", err)
	}
}

// randomSuffix keeps concurrent or repeated runs from colliding on the
// well-known, absolute paths namespaces are pinned at.
func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(b[:])
}
