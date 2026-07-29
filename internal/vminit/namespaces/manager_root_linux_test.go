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

package namespaces

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

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
// Scope note for the PID namespace: createPID anchors it by re-execing
// /proc/self/exe, which here is this test binary rather than vminitd, so the
// anchor exits immediately instead of persisting. That is enough to exercise
// the bind-mount and teardown mechanics asserted below, but it does not cover
// the anchor actually holding the namespace open over time — that is covered
// end to end by the shared-PID-namespace conformance test running against a
// real guest.
func TestManagerCreateDeleteRoundTrip(t *testing.T) {
	requireNamespacePrivileges(t)

	ctx := context.Background()
	id := "nerdbox-test-" + randomSuffix(t)
	types := []Type{TypeNetwork, TypeIPC, TypePID}

	var m Manager
	t.Cleanup(func() {
		// Best effort, in case an assertion below fails before Delete runs.
		_ = m.Delete(ctx, id, nil)
	})

	paths, err := m.Create(ctx, id, types)
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
	// anything new, and must report the same paths.
	again, err := m.Create(ctx, id, types)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	for _, typ := range types {
		if again[typ] != paths[typ] {
			t.Errorf("%s path changed across calls: %q then %q", typ, paths[typ], again[typ])
		}
	}

	// Requesting a subset must not disturb the rest.
	subset, err := m.Create(ctx, id, []Type{TypeIPC})
	if err != nil {
		t.Fatalf("subset Create: %v", err)
	}
	if len(subset) != 1 || subset[TypeIPC] != paths[TypeIPC] {
		t.Errorf("subset Create = %v, want just the IPC path %q", subset, paths[TypeIPC])
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

	// Delete must be idempotent.
	if err := m.Delete(ctx, id, nil); err != nil {
		t.Errorf("second Delete: %v", err)
	}
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

	if _, err := m.Create(ctx, id, []Type{TypeIPC}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, typ := range []Type{TypeNetwork, TypePID} {
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
