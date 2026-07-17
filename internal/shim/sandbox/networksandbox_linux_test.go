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
	"os"
	"path/filepath"
	"testing"
)

// TestLinuxOpenNetworkSandbox_Empty verifies an empty path returns
// NoNetworkSandbox rather than attempting to open anything.
func TestLinuxOpenNetworkSandbox_Empty(t *testing.T) {
	ns, err := linuxOpenNetworkSandbox("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ns.(NoNetworkSandbox); !ok {
		t.Fatalf("expected NoNetworkSandbox, got %T", ns)
	}
}

// TestLinuxOpenNetworkSandbox_PlainFile verifies that a plain regular file
// (not backed by nsfs) is still accepted rather than rejected -- this is
// the technique shimtest's non-root suite uses to pin a fake network
// sandbox without requiring CAP_SYS_ADMIN or kernel bind-mount support.
func TestLinuxOpenNetworkSandbox_PlainFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network-sandbox")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		t.Fatalf("create test file: %v", err)
	}
	f.Close()

	ns, err := linuxOpenNetworkSandbox(path)
	if err != nil {
		t.Fatalf("unexpected error opening a plain-file network sandbox: %v", err)
	}
	defer ns.Close()

	if ns.Path() != path {
		t.Fatalf("Path() = %q, want %q", ns.Path(), path)
	}
}

// TestLinuxOpenNetworkSandbox_Missing verifies that a nonexistent path
// returns an error rather than silently succeeding.
func TestLinuxOpenNetworkSandbox_Missing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := linuxOpenNetworkSandbox(path); err == nil {
		t.Fatalf("expected error for a nonexistent network sandbox path")
	}
}
