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
	"os"
	"path/filepath"
	"testing"
)

// TestUnshareRetainsFailedMountForRetry verifies that a mount point Unshare
// fails to unmount is kept in s.mounts, rather than discarded, so a later
// retry (another Unshare or UnshareAll call) still knows to try it again.
// Losing that bookkeeping would permanently strand the mount: nothing else
// records where it is.
//
// This runs only as non-root: as a regular user, unmount(2) on any target —
// mounted or not — fails with EPERM (lacking CAP_SYS_ADMIN), which
// mount.UnmountAll propagates as a real error. Run as root, unmounting an
// ordinary directory that was never mounted returns EINVAL, which
// mount.UnmountAll deliberately squelches to a nil (success) return — so
// this specific failure mode can't be exercised there without an actual
// mount to break.
func TestUnshareRetainsFailedMountForRetry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root: see doc comment")
	}

	const containerID = "test-container"
	root := t.TempDir()
	target := filepath.Join(root, containerID, "rootfs")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	s := &SharedFS{
		root:   root,
		mounts: map[string][]string{containerID: {target}},
	}

	if err := s.unshare(context.Background(), containerID); err == nil {
		t.Fatal("unshare: got nil error, want one (unmount as non-root should fail with EPERM)")
	}

	s.mu.Lock()
	got := s.mounts[containerID]
	s.mu.Unlock()
	if len(got) != 1 || got[0] != target {
		t.Fatalf("s.mounts[%q] = %v, want [%q] (the failed mount should be retained for retry)", containerID, got, target)
	}

	// The container directory must survive too: removing it while a mount
	// under it is still (from our bookkeeping's point of view) unresolved
	// would orphan whatever that mount was ultimately backing.
	if _, err := os.Stat(filepath.Join(root, containerID)); err != nil {
		t.Fatalf("container dir removed despite a retained failed mount: %v", err)
	}
}
