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

// This file contains SandboxSuite member-container workload tests that
// require Linux kernel features (network namespaces).

package shimtest

import (
	"testing"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
)

// testNetworkSandboxHeldOpen verifies that the shim holds the host-side
// network sandbox open for the lifetime of the sandbox and releases it after
// the sandbox stops.
//
// The API contract: a shim that receives a non-empty netns_path in
// CreateSandboxRequest must pin the network sandbox resource (e.g. a Linux
// network namespace bind-mount) for the duration of the sandbox.  This allows
// CNI and other host-side tools to inspect or manipulate the network sandbox
// while the sandbox is running.  After StopSandbox the shim must release its
// pin so that the caller can unmount the bind-mount and reclaim the resource.
//
// The test creates a real host-side network sandbox (bind-mounted netns),
// passes its path to CreateSandbox, asserts the path is reachable while the
// sandbox is ready, then stops the sandbox and verifies the state transition.
func (s *SandboxSuite) testNetworkSandboxHeldOpen(t *testing.T) {
	nsPath := createNetworkSandbox(t)

	sandboxID := containerID(t)
	env := startSandboxShimWithNetworkSandbox(t, s.cfg, sandboxID, nsPath)

	// While the sandbox is running the bind-mount must still be reachable.
	// A missing path means the shim (or something else) unmounted it
	// prematurely — violating the hold-open contract.
	if !networkSandboxIsOpen(nsPath) {
		t.Fatal("network sandbox disappeared while sandbox is running; shim must hold it open")
	}
	t.Logf("network sandbox %q is reachable while sandbox is running", nsPath)

	// Run a member container to confirm the sandbox is fully operational.
	cid := createContainerInSandbox(t, env, []string{"/bin/echo", "netns-ok"})
	readContainerOutput(t, env, cid, "netns-ok", 30*time.Second)
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid})     //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid}) //nolint:errcheck

	// Stop the sandbox explicitly so we can inspect the final state.
	if _, err := env.sc.StopSandbox(env.ctx, &sandboxAPI.StopSandboxRequest{
		SandboxID: sandboxID,
	}); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}

	if err := waitForSandboxStatus(env.ctx, env.sc, sandboxID, sandboxStateNotReady, 10*time.Second); err != nil {
		t.Errorf("SandboxStatus state after stop: %v", err)
	}
	t.Log("network sandbox held open while running; sandbox stopped cleanly")
}

// testNetworkSandboxPathInStatus verifies that SandboxStatus may report the
// network sandbox path in its Info map under "networkSandboxPath".
//
// The API contract: the base sandbox TTRPC protocol does not mandate specific
// Info keys.  A shim that accepts a network sandbox path is encouraged to
// expose it in Info so callers can confirm which network resource is pinned
// without side-channel lookups.  The test treats absence of the key as an
// informational result rather than a hard failure.
func (s *SandboxSuite) testNetworkSandboxPathInStatus(t *testing.T) {
	nsPath := createNetworkSandbox(t)

	sandboxID := containerID(t)
	env := startSandboxShimWithNetworkSandbox(t, s.cfg, sandboxID, nsPath)

	_, info := sandboxStatusInfo(t, env)
	reported := info["networkSandboxPath"]
	if reported == "" {
		t.Logf("SandboxStatus.Info does not include 'networkSandboxPath' (optional field); info=%v", info)
		return
	}
	if reported != nsPath {
		t.Errorf("SandboxStatus.Info[networkSandboxPath]: got %q, want %q", reported, nsPath)
	}
	t.Logf("SandboxStatus.Info[networkSandboxPath]=%q (matches provided path)", reported)
}
