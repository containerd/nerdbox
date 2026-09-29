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

package sandbox

import (
	"context"
	"strings"
	"testing"
)

// TestGuestRootfsPath_UsesForwardSlashes verifies GuestRootfsPath builds an
// in-guest (always Linux) path using '/' separators regardless of the host
// OS this test runs on. The guest is always Linux even when this shim runs
// on a Windows host, where filepath.Join would use '\' and produce a path
// the guest could never use.
func TestGuestRootfsPath_UsesForwardSlashes(t *testing.T) {
	got := GuestRootfsPath("abc123")
	want := "/run/containers/abc123/rootfs"
	if got != want {
		t.Fatalf("GuestRootfsPath() = %q, want %q", got, want)
	}
	if strings.ContainsRune(got, '\\') {
		t.Fatalf("GuestRootfsPath() contains a backslash: %q", got)
	}
}

// TestGuestVolumePath_UsesForwardSlashes verifies GuestVolumePath builds an
// in-guest path using '/' separators. See TestGuestRootfsPath_UsesForwardSlashes.
func TestGuestVolumePath_UsesForwardSlashes(t *testing.T) {
	got := GuestVolumePath("abc123", 2)
	want := "/run/containers/abc123/volumes/2"
	if got != want {
		t.Fatalf("GuestVolumePath() = %q, want %q", got, want)
	}
	if strings.ContainsRune(got, '\\') {
		t.Fatalf("GuestVolumePath() contains a backslash: %q", got)
	}
}

// TestValidateContainerID checks the specific inputs that would otherwise
// let a container ID escape SharedFS.root (on the host) or
// GuestContainersDir (in the guest) once joined onto it — see
// ShareRootfs/ShareVolume/Unshare, which all reject an ID via this
// function before constructing any path from it.
func TestValidateContainerID(t *testing.T) {
	bad := []string{"", ".", "..", "../x", "a/../../b", "a/b", "/etc/passwd", "a\x00b"}
	for _, id := range bad {
		if err := validateContainerID(id); err == nil {
			t.Errorf("validateContainerID(%q) = nil, want an error", id)
		}
	}

	good := []string{"abc123", "test-container_1", "a.b"}
	for _, id := range good {
		if err := validateContainerID(id); err != nil {
			t.Errorf("validateContainerID(%q) = %v, want nil", id, err)
		}
	}
}

// TestSharedFSRejectsInvalidContainerID verifies that ShareRootfs,
// ShareVolume, and Unshare all reject a malicious container ID before
// doing anything else — in particular, before ever reaching a platform
// implementation that would construct a filesystem path from it. Using an
// ID that would escape s.root if unchecked (rather than just checking the
// error's presence) makes this a regression test for the actual path
// traversal, not just for validateContainerID being called at all.
func TestSharedFSRejectsInvalidContainerID(t *testing.T) {
	const evil = "../evil"
	s := &SharedFS{root: t.TempDir(), mounts: make(map[string][]string)}
	ctx := context.Background()

	if _, err := s.ShareRootfs(ctx, evil, nil); err == nil {
		t.Error("ShareRootfs with a path-traversal container id: got nil error, want one")
	}
	if _, err := s.ShareVolume(ctx, evil, 0, "/tmp", true); err == nil {
		t.Error("ShareVolume with a path-traversal container id: got nil error, want one")
	}
	if err := s.Unshare(ctx, evil); err == nil {
		t.Error("Unshare with a path-traversal container id: got nil error, want one")
	}
}
