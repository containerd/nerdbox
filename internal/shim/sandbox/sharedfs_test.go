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
