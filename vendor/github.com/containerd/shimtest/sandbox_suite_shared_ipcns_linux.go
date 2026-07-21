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
	"strings"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// testMemberContainersShareIPC verifies that member containers of the
// same sandbox can share an IPC namespace: a SysV shared memory segment
// created by one member container is visible — by its well-known key —
// to a second, independently created member container.
//
// SysV IPC objects are chosen (rather than, say, a shared file) because
// their visibility is governed entirely by the process's IPC namespace,
// independent of mount namespace or any bind-mounted directory. A
// successful cross-container round trip through the same key is
// conclusive proof of a shared IPC namespace specifically, not an
// artifact of some other sharing mechanism.
//
// The API contract: when a member container's OCI spec carries a host
// path on its IPC namespace entry, the shim must place that container
// in an IPC namespace shared with its sandbox peers (e.g. this is how a
// caller expresses Kubernetes' default of always sharing one IPC
// namespace across a pod's containers). This test only observes the
// externally visible result and does not assume any particular
// mechanism a shim uses to provide it. It intentionally uses a
// placeholder host path (see withSandboxCtrNamespace) since only a
// live host has an actual sandbox PID to put there.
func (s *SandboxSuite) testMemberContainersShareIPC(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	const (
		shmKey = "424242"
		marker = "shared-ipcns-ok"
	)

	writerCID := createContainerInSandbox(t, env, []string{"/bin/shmwrite", shmKey, marker},
		withSandboxCtrNamespace(specs.IPCNamespace, "/proc/1/ns/ipc"))

	writerWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: writerCID})
	if err != nil {
		t.Fatalf("Task.Wait writer: %v", err)
	}
	if writerWait.GetExitStatus() != 0 {
		t.Fatalf("writer container exit status: got %d, want 0", writerWait.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: writerCID}) //nolint:errcheck

	// The shm segment created by the writer persists in the IPC
	// namespace after the writer container exits (nothing calls
	// IPC_RMID on it), so there is no ordering requirement beyond the
	// writer having already exited.
	readerCID := createContainerInSandbox(t, env, []string{"/bin/shmread", shmKey},
		withSandboxCtrNamespace(specs.IPCNamespace, "/proc/1/ns/ipc"))

	out := readContainerOutput(t, env, readerCID, marker, 30*time.Second)
	if !strings.Contains(out, marker) {
		t.Fatalf("shmread output did not contain marker %q: %q", marker, out)
	}

	readerWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: readerCID})
	if err != nil {
		t.Fatalf("Task.Wait reader: %v", err)
	}
	if readerWait.GetExitStatus() != 0 {
		t.Fatalf("reader container exit status: got %d, want 0", readerWait.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: readerCID}) //nolint:errcheck

	t.Log("member containers share an IPC namespace: shared memory segment visible across containers")
}
