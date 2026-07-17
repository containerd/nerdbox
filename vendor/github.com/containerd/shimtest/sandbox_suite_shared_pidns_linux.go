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

// testMemberContainersSharePID verifies that member containers of the
// same sandbox can share a PID namespace: a process started by one
// member container is visible — by PID and argv — to a second,
// independently created member container.
//
// The API contract: when a member container's OCI spec carries a host
// path on its PID namespace entry, the shim must place that container
// in a PID namespace shared with its sandbox peers rather than a fresh,
// isolated one (e.g. this is how a caller expresses Kubernetes'
// shareProcessNamespace: true or hostPID: true for a pod). This test
// only observes the externally visible result — cross-container process
// visibility via /proc — and does not assume any particular mechanism a
// shim uses to provide it (a real shared PID namespace, or any other
// approach). It intentionally uses a placeholder host path (see
// withSandboxCtrNamespace) since only a live host has an actual sandbox
// PID to put there; the shim's job is to recognize that a host path was
// requested at all and substitute its own equivalent, not to interpret
// the specific value.
func (s *SandboxSuite) testMemberContainersSharePID(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	const marker = "pid-share-marker-forever"

	sentinelCID := createContainerInSandbox(t, env, []string{"/bin/forever", marker},
		withSandboxCtrNamespace(specs.PIDNamespace, "/proc/1/ns/pid"))

	// Give the sentinel process a moment to actually start before the
	// scanner container looks for it; there is no synchronous "ready"
	// signal available across two containers created independently via
	// the Task API.
	time.Sleep(200 * time.Millisecond)

	scannerCID := createContainerInSandbox(t, env, []string{"/bin/pidscan"},
		withSandboxCtrNamespace(specs.PIDNamespace, "/proc/1/ns/pid"))

	out := readContainerOutput(t, env, scannerCID, marker, 30*time.Second)
	if !strings.Contains(out, marker) {
		t.Fatalf("pidscan output did not contain sentinel marker %q: %q", marker, out)
	}

	scannerWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: scannerCID})
	if err != nil {
		t.Fatalf("Task.Wait scanner: %v", err)
	}
	if scannerWait.GetExitStatus() != 0 {
		t.Fatalf("scanner container exit status: got %d, want 0", scannerWait.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: scannerCID}) //nolint:errcheck

	if _, err := env.tc.Kill(env.ctx, &taskAPI.KillRequest{ID: sentinelCID, Signal: 9, All: true}); err != nil {
		t.Fatalf("Task.Kill sentinel: %v", err)
	}
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: sentinelCID})     //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: sentinelCID}) //nolint:errcheck

	t.Log("member containers share a PID namespace: scanner observed the sentinel process's argv")
}
