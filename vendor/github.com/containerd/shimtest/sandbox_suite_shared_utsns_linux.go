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

// requireSandboxHostnameCapability probes, independently of any
// namespace-sharing behavior, whether the shim honors a member
// container's request for CAP_SYS_ADMIN by running "hostname <name>"
// once in an otherwise-unshared container. If the set fails, the
// calling test is skipped rather than failed: granting a container's
// requested Linux capabilities is a separate contract from namespace
// sharing (which is what UTS tests in this file actually check), and a
// shim that doesn't support capability requests at all shouldn't be
// penalized on a contract it was never asked to implement here.
func requireSandboxHostnameCapability(t *testing.T, env *sandboxEnv) {
	t.Helper()

	cid := createContainerInSandbox(t, env, []string{"/bin/hostname", "cap-probe-" + randomSuffix()},
		withSandboxCtrOCIOpts(withCapabilities("CAP_SYS_ADMIN")))
	waitResp, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatalf("Task.Wait capability probe: %v", err)
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid}) //nolint:errcheck
	if waitResp.GetExitStatus() != 0 {
		t.Skipf("shim did not grant the requested CAP_SYS_ADMIN to the container (hostname set failed); cannot verify UTS namespace behavior")
	}
}

// setSandboxHostname creates a member container that sets the UTS
// namespace hostname via the standard "hostname <name>" command,
// applying opts (e.g. withSandboxCtrNamespace(UTSNamespace, ...) to
// join a shared UTS namespace) to its spec, and waits for it to exit.
//
// Callers must call requireSandboxHostnameCapability first: with the
// capability precondition already verified separately, a non-zero exit
// here is treated as a hard test failure rather than a skip.
//
// The standard "hostname <name>" exits immediately and silently on a
// successful set rather than holding the namespace open itself (see
// cmdHostname), so a caller that later observes the change is also
// proving the namespace — and its hostname — outlives the process that
// set it, not merely that a still-running setter's own namespace is
// visible.
func setSandboxHostname(t *testing.T, env *sandboxEnv, hostname string, opts ...func(*sandboxCtrSpec)) {
	t.Helper()

	opts = append(opts, withSandboxCtrOCIOpts(withCapabilities("CAP_SYS_ADMIN")))
	cid := createContainerInSandbox(t, env, []string{"/bin/hostname", hostname}, opts...)

	waitResp, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatalf("Task.Wait hostname setter: %v", err)
	}
	if waitResp.GetExitStatus() != 0 {
		t.Fatalf("hostname setter exit status: got %d, want 0 (CAP_SYS_ADMIN already verified available)", waitResp.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid}) //nolint:errcheck
}

// readSandboxHostname creates a member container that prints its UTS
// namespace hostname via the standard, argument-less "hostname"
// command, applying opts to its spec, waits for it to exit, and
// returns the trimmed hostname it reported.
func readSandboxHostname(t *testing.T, env *sandboxEnv, opts ...func(*sandboxCtrSpec)) string {
	t.Helper()

	cid := createContainerInSandbox(t, env, []string{"/bin/hostname"}, opts...)
	waitResp, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatalf("Task.Wait hostname reader: %v", err)
	}
	if waitResp.GetExitStatus() != 0 {
		t.Fatalf("hostname reader exit status: got %d, want 0", waitResp.GetExitStatus())
	}
	// Allow a moment for the last of stdout to drain after exit (see
	// containerOutputSnapshot).
	time.Sleep(50 * time.Millisecond)
	out := strings.TrimSpace(containerOutputSnapshot(t, env, cid))
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid}) //nolint:errcheck
	return out
}

// testMemberContainersShareUTS verifies that member containers of the
// same sandbox can share a UTS namespace: a hostname change made by one
// member container via the standard "hostname <name>" command is
// visible — via the kernel's reported hostname, not a file or
// environment variable — to a second, independently created member
// container, even after the container that made the change has exited.
//
// The API contract: when a member container's OCI spec carries a host
// path on its UTS namespace entry (e.g. this is how a caller expresses
// Kubernetes' default of sharing one hostname across a pod's
// containers), the shim must place that container in a UTS namespace
// shared with its sandbox peers rather than a fresh, isolated one, and
// that shared namespace must be owned by the sandbox rather than tied
// to the lifetime of whichever container last changed its hostname.
// This test only observes the externally visible result and does not
// assume any particular mechanism a shim uses to provide it. It
// intentionally uses a placeholder host path (see
// withSandboxCtrNamespace) since only a live host has an actual
// sandbox PID to put there.
func (s *SandboxSuite) testMemberContainersShareUTS(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	requireSandboxHostnameCapability(t, env)

	hostname := "shared-uts-" + randomSuffix()
	setSandboxHostname(t, env, hostname, withSandboxCtrNamespace(specs.UTSNamespace, "/proc/1/ns/uts"))

	got := readSandboxHostname(t, env, withSandboxCtrNamespace(specs.UTSNamespace, "/proc/1/ns/uts"))
	if got != hostname {
		t.Fatalf("reader hostname: got %q, want %q", got, hostname)
	}

	t.Log("member containers share a UTS namespace: hostname change outlived the container that set it and was visible to a peer")
}

// testMemberContainersUTSNotShared verifies the converse of
// testMemberContainersShareUTS: a member container that does not
// request UTS sharing must not observe a peer's hostname change, even
// though both containers belong to the same sandbox.
//
// The API contract mirrors testMemberContainersSharePID's converse and
// testMemberContainersDevShmNotSharedWithoutIPC: the shim must key UTS
// namespace sharing off the container's own UTS-namespace-sharing
// signal, not off simply being a member of the same sandbox.
func (s *SandboxSuite) testMemberContainersUTSNotShared(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	requireSandboxHostnameCapability(t, env)

	hostname := "should-not-be-visible-" + randomSuffix()
	setSandboxHostname(t, env, hostname, withSandboxCtrNamespace(specs.UTSNamespace, "/proc/1/ns/uts"))

	// No withSandboxCtrNamespace(UTSNamespace, ...): this container does
	// not request UTS sharing, so it must get its own, unaffected UTS
	// namespace.
	got := readSandboxHostname(t, env)
	if got == hostname {
		t.Fatalf("reader saw writer's hostname %q despite neither container sharing UTS", hostname)
	}

	t.Log("member container not sharing UTS correctly did not observe a peer's hostname change")
}
