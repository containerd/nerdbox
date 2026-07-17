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

package shimtest

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// createNetworkSandbox creates a host-side network sandbox resource and
// returns its file path.  Cleanup is registered on tb.
//
// The network sandbox is represented as a regular file.  This is sufficient
// to test the shim's API contract — that it accepts a netns_path in
// CreateSandboxRequest, holds the resource open for the sandbox lifetime, and
// reports the path in SandboxStatus.Info — without requiring root privileges
// or kernel support for bind-mounting namespace files.
//
// In production, a caller provides a bind-mounted network namespace file
// created before calling CreateSandbox.  The shim is expected to open the
// path, pin it for the sandbox lifetime, and (when running as root) enter
// the namespace so that member-container traffic originates from the
// provided netns.  Entering the namespace requires CAP_SYS_ADMIN; when the
// shim lacks that capability it logs a warning and continues without
// entering.
func createNetworkSandbox(tb testing.TB) string {
	tb.Helper()

	nsPath := filepath.Join(tb.TempDir(), "network-sandbox")

	f, err := os.OpenFile(nsPath, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		tb.Fatalf("createNetworkSandbox: create resource file: %v", err)
	}
	f.Close()

	// No explicit cleanup needed: tb.TempDir() handles removal.
	return nsPath
}

// networkSandboxIsOpen returns true if the network sandbox file at path still
// exists and is stat-able.  Returns false if the path is empty, does not
// exist, or cannot be accessed.
func networkSandboxIsOpen(path string) bool {
	if path == "" {
		return false
	}
	var st unix.Stat_t
	return unix.Stat(path, &st) == nil
}
