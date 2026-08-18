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

// testMemberContainersSharePIDKillScopedToOwnContainer verifies the
// converse safety property to testMemberContainersSharePID: sharing a PID
// namespace makes a peer's processes *visible*, but Task.Kill and
// Task.Pids remain scoped to the container named in the request and never
// affect or report a peer's processes, even though both containers' PIDs
// live in the same namespace and are visible to each other via /proc.
//
// The API contract: Task.Kill and Task.Pids identify processes by
// container ID (and, for Kill, ExecID) — never by a raw PID — so a shim
// must resolve a request against that specific container's own tracked
// process(es), not by number within whatever PID namespace the container
// happens to be in. A caller has no way to name a PID at all through this
// API, so this test's job is to confirm a shim doesn't reintroduce that
// possibility through some other means (e.g. resolving Kill by scanning a
// shared namespace for a matching argv or similar).
func (s *SandboxSuite) testMemberContainersSharePIDKillScopedToOwnContainer(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	const (
		victimMarker = "pid-kill-scope-victim"
		peerMarker   = "pid-kill-scope-peer"
	)

	victimCID := createContainerInSandbox(t, env, []string{"/bin/forever", victimMarker},
		withSandboxCtrNamespace(specs.PIDNamespace, "/proc/1/ns/pid"))
	peerCID := createContainerInSandbox(t, env, []string{"/bin/forever", peerMarker},
		withSandboxCtrNamespace(specs.PIDNamespace, "/proc/1/ns/pid"))

	// Give both a moment to actually start (see testMemberContainersSharePID).
	time.Sleep(200 * time.Millisecond)

	// Confirm sharing actually took effect before testing scoping: a
	// scanner that can't see the peer at all would make the rest of this
	// test meaningless (scoping would trivially "work" for the wrong
	// reason).
	scannerCID := createContainerInSandbox(t, env, []string{"/bin/pidscan"},
		withSandboxCtrNamespace(specs.PIDNamespace, "/proc/1/ns/pid"))
	out := readContainerOutput(t, env, scannerCID, peerMarker, 30*time.Second)
	if !strings.Contains(out, peerMarker) {
		t.Fatalf("pidscan output did not contain peer marker %q: %q (sharing did not take effect)", peerMarker, out)
	}
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: scannerCID})     //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: scannerCID}) //nolint:errcheck

	// Kill the victim with All: true -- the broadest kill request the API
	// allows -- and confirm the peer, sharing the same PID namespace, is
	// unaffected.
	if _, err := env.tc.Kill(env.ctx, &taskAPI.KillRequest{ID: victimCID, Signal: 9, All: true}); err != nil {
		t.Fatalf("Task.Kill victim: %v", err)
	}
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: victimCID})     //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: victimCID}) //nolint:errcheck

	peerPids, err := env.tc.Pids(env.ctx, &taskAPI.PidsRequest{ID: peerCID})
	if err != nil {
		t.Fatalf("Task.Pids peer: %v", err)
	}
	if len(peerPids.GetProcesses()) == 0 {
		t.Fatal("peer container reports no processes after killing the victim; it should be unaffected")
	}

	if _, err := env.tc.Kill(env.ctx, &taskAPI.KillRequest{ID: peerCID, Signal: 9, All: true}); err != nil {
		t.Fatalf("Task.Kill peer: %v", err)
	}
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: peerCID})     //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: peerCID}) //nolint:errcheck

	t.Log("Task.Kill and Task.Pids stayed scoped to their own container despite a shared PID namespace")
}
