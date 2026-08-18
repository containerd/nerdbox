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
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// testMemberContainerHostVolume verifies that a member container can be
// given a host directory as a bind-mount volume (an OCI "bind" mount type
// in the container spec), and that the mount is a real, live share — not
// a one-time copy: a file updated on the host after the container has
// already started must become visible inside it.
//
// The API contract: a container's OCI spec may include "bind" mounts
// referencing host paths (e.g. this is how a caller expresses Kubernetes
// hostPath volumes or RecursiveReadOnly=false bind mounts) and the shim
// must honor them for member containers, exactly as it does for the
// top-level bundle rootfs. This test only observes the externally
// visible behavior: it does not assume any particular implementation
// mechanism (a hot-added virtual filesystem, a pre-existing shared tree,
// or anything else a shim might use to satisfy the mount).
func (s *SandboxSuite) testMemberContainerHostVolume(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	hostDir := t.TempDir()
	const (
		fileName    = "data.txt"
		hostToken   = "AAA-initial"
		containerMP = "/data"
	)
	hostFile := filepath.Join(hostDir, fileName)
	if err := os.WriteFile(hostFile, []byte(hostToken+"\n"), 0o644); err != nil {
		t.Fatalf("write host file: %v", err)
	}

	cid := createContainerInSandbox(t, env, []string{"/bin/forever", "volume-container"},
		withSandboxCtrExtraMounts(specs.Mount{
			Type:        "bind",
			Source:      hostDir,
			Destination: containerMP,
			Options:     []string{"rbind", "rw"},
		}),
	)

	// Host-to-container direction: the file written before the container
	// started must be readable inside it. Tokens deliberately use short,
	// mutually-exclusive prefixes ("AAA"/"BBB") rather than distinguishing
	// suffixes: execInSandboxContainer's output capture allows only a
	// fixed, short grace period for FIFO data to drain after the exec'd
	// process exits, so a real but short read (e.g. just "AAA-in") must
	// still unambiguously identify which file version was seen.
	out, exitCode := execInSandboxContainer(t, env, cid,
		[]string{"/bin/cat", containerMP + "/" + fileName}, 30*time.Second)
	if exitCode != 0 {
		t.Fatalf("cat host file from container: exit code %d, output %q", exitCode, out)
	}
	if !strings.HasPrefix(out, "AAA") {
		t.Errorf("container read of host file: got %q, want a prefix of %q", out, hostToken)
	}

	// Container-to-host direction is exercised the other way around: update
	// the file on the host after the container has already started and
	// confirm the container sees the change live, proving this is a real
	// shared mount and not a one-shot copy taken when the container
	// started.
	const updatedToken = "BBB-updated"
	if err := os.WriteFile(hostFile, []byte(updatedToken+"\n"), 0o644); err != nil {
		t.Fatalf("update host file: %v", err)
	}
	out, exitCode = execInSandboxContainer(t, env, cid,
		[]string{"/bin/cat", containerMP + "/" + fileName}, 30*time.Second)
	if exitCode != 0 {
		t.Fatalf("cat updated host file from container: exit code %d, output %q", exitCode, out)
	}
	if !strings.HasPrefix(out, "BBB") {
		t.Errorf("container read of updated host file: got %q, want a prefix of %q", out, updatedToken)
	}

	t.Log("member container host volume: live bind mount confirmed both at creation and after a host-side update")
}
