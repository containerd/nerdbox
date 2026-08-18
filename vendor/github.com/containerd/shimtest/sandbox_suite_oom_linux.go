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
	"syscall"
	"testing"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	tasktypes "github.com/containerd/containerd/api/types/task"
)

// testMemberContainerOOMIsolation verifies that the kernel OOM-killing
// one member container is scoped to that container: a peer member
// container and the sandbox itself must be unaffected.
//
// The API contract: a per-container memory limit (set via the
// container's OCI spec, exactly as OOMSuite's standalone test does) is
// a property of that one container's cgroup. The shim must not let an
// OOM kill inside one container's cgroup propagate to, or otherwise
// disturb, a sibling member container or the sandbox VM/process group
// hosting them both. This is the OOM-specific counterpart to
// testContainerLifecycleIndependence, which covers the same "one
// container's fate must not affect its peers or the sandbox" contract
// for a graceful exit rather than a kernel-forced kill.
func (s *SandboxSuite) testMemberContainerOOMIsolation(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	// Start a long-lived peer with no memory limit.
	peerCID := createContainerInSandbox(t, env, []string{"/bin/forever", "oom-peer"})

	// Start the victim with a tight memory limit and a memory-hungry
	// workload, matching OOMSuite's standalone test.
	victimCID := createContainerInSandbox(t, env, []string{"/bin/memhog"},
		withSandboxCtrOCIOpts(withMemoryLimit(128*1024*1024)))

	victimWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: victimCID})
	if err != nil {
		t.Fatalf("Task.Wait victim: %v", err)
	}
	const sigkillExit = 128 + uint32(syscall.SIGKILL)
	if victimWait.GetExitStatus() != sigkillExit {
		t.Fatalf("victim container exit status: got %d, want %d (SIGKILL from OOM killer)",
			victimWait.GetExitStatus(), sigkillExit)
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: victimCID}) //nolint:errcheck

	// The peer, in its own unrestricted cgroup, must still be running.
	stateResp, err := env.tc.State(env.ctx, &taskAPI.StateRequest{ID: peerCID})
	if err != nil {
		t.Fatalf("State for peer after victim OOM kill: %v", err)
	}
	if stateResp.GetStatus() != tasktypes.Status_RUNNING {
		t.Errorf("peer container status after victim's OOM kill: got %v, want RUNNING", stateResp.GetStatus())
	}

	// The sandbox itself must also still be running.
	status, err := env.sc.SandboxStatus(env.ctx, &sandboxAPI.SandboxStatusRequest{SandboxID: sandboxID})
	if err != nil {
		t.Fatalf("SandboxStatus after victim's OOM kill: %v", err)
	}
	if status.GetState() != sandboxStateReady {
		t.Errorf("sandbox state after victim's OOM kill: got %q, want %q", status.GetState(), sandboxStateReady)
	}

	t.Log("peer still running and sandbox intact after a sibling container's OOM kill")

	env.tc.Kill(env.ctx, &taskAPI.KillRequest{ID: peerCID, Signal: uint32(syscall.SIGKILL), All: true}) //nolint:errcheck
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: peerCID})                                             //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: peerCID})                                         //nolint:errcheck
}
