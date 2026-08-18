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

// This file contains a SandboxSuite test that verifies container network
// traffic is actually scoped to the network sandbox the shim was given, as
// opposed to merely holding the resource's path open. It requires root to
// create a real network namespace and network interfaces, and is skipped
// otherwise.

package shimtest

import (
	"net"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
)

// testContainerTrafficScopedToNetworkSandbox verifies that when a sandbox is
// given a network sandbox (a non-empty netns_path in CreateSandboxRequest),
// a member container's outbound network traffic actually originates from
// within that network sandbox, rather than from whatever network context
// the shim process happens to run in.
//
// The API contract: passing a non-empty netns_path in CreateSandboxRequest
// must result in member container traffic being scoped to that network
// sandbox — regardless of the mechanism the shim uses internally (native
// namespace membership, a virtual NIC bridged into the namespace, or any
// other approach). This test only observes the externally visible result:
// a container must be able to reach an endpoint that exists only inside the
// provided network sandbox, exactly as any container workload reaching a
// service scoped to that network sandbox would.
//
// Test topology: a veth pair is created with both ends inside a real network
// namespace, giving one end an address on a subnet that has no route from
// outside that namespace (see realNetworkSandbox.attachVeth). A listener is
// bound to that address from inside the namespace. The sandbox is started
// with the namespace's path, and a member container runs nc(1) in TCP mode
// (nc <host> <port>) to reach the listener's address, sending a token via
// its stdin FIFO and printing the echoed response to stdout. Since the
// address is unreachable from any context other than the namespace itself,
// a successful round trip is only possible if the container's traffic
// actually originates there.
//
// Requires root (CAP_SYS_ADMIN) to create a network namespace and attach
// network interfaces; skipped otherwise.
func (s *SandboxSuite) testContainerTrafficScopedToNetworkSandbox(t *testing.T) {
	netns := createRealNetworkSandbox(t)
	addr := netns.attachVeth(t)
	const port = "9191"
	endpoint := addr + ":" + port

	// Sanity check: confirm the address really is unreachable from the
	// namespace this test process runs in, so that a later successful
	// connection can only be explained by the container's traffic
	// originating inside the sandbox namespace.
	probeUnreachableFromCurrentNamespace(t, endpoint)

	done := listenAndEchoOnceInNetns(t, netns.path, endpoint)

	sandboxID := containerID(t)
	env := startSandboxShimWithNetworkSandbox(t, s.cfg, sandboxID, netns.path)

	const token = "netns-scoped-ok"
	cid := createContainerInSandbox(t, env, []string{"/bin/nc", addr, port}, withSandboxCtrStdin())
	writeContainerStdin(t, env, cid, token+"\n")
	readContainerOutput(t, env, cid, token, 30*time.Second)

	waitResp, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatalf("Task.Wait: %v", err)
	}
	if waitResp.GetExitStatus() != 0 {
		t.Fatalf("container exit status: got %d, want 0 "+
			"(container could not reach the network-sandbox-scoped endpoint; "+
			"its traffic may not be originating inside the provided network sandbox)",
			waitResp.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid}) //nolint:errcheck

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("network-sandbox-scoped endpoint did not observe a successful exchange: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("network-sandbox-scoped endpoint did not observe a connection from the container within 5s")
	}

	t.Log("container traffic is scoped to the provided network sandbox")
}

// testInboundToNetnsScopedListener verifies the inbound (listen) side of the
// network-sandbox contract: a member container that binds a TCP listener must
// be reachable by a caller connecting from within the same network sandbox.
//
// The API contract: when a sandbox is given a non-empty netns_path in
// CreateSandboxRequest, a member container's inbound TCP listener must be
// accessible from within that network sandbox — not only from the root
// namespace. This is the mirror of testContainerTrafficScopedToNetworkSandbox:
// where that test verifies outbound traffic stays inside the sandbox, this
// test verifies that inbound connections from inside the sandbox reach the
// container listener.
//
// Test topology: the same veth-pair-inside-a-real-netns setup from
// testContainerTrafficScopedToNetworkSandbox, except the roles are reversed.
// A member container runs echosrv (which prints its bound port to stdout then
// accepts one connection and echoes). Once the port is known, the test enters
// the sandbox namespace on a locked OS thread and dials the container from
// inside it. Since the veth addresses are unreachable from outside the
// namespace, a successful round trip proves the listener is inside the sandbox.
//
// Requires root (CAP_SYS_ADMIN) to create a network namespace and attach
// network interfaces; skipped otherwise.
func (s *SandboxSuite) testInboundToNetnsScopedListener(t *testing.T) {
	netns := createRealNetworkSandbox(t)
	vethAddr := netns.attachVeth(t)

	// Sanity check: the veth address is unreachable from our namespace.
	probeUnreachableFromCurrentNamespace(t, net.JoinHostPort(vethAddr, "1"))

	sandboxID := containerID(t)
	env := startSandboxShimWithNetworkSandbox(t, s.cfg, sandboxID, netns.path)

	// Start echosrv 0 (ephemeral port). echosrv prints the bound port to
	// stdout before accepting, so readContainerOutput will capture it.
	listenerCID := createContainerInSandbox(t, env, []string{"/bin/echosrv", "0"})

	// Wait for echosrv to print the port to stdout. The port appears as the
	// first (and only pre-accept) line; give it 15 s which is generous for
	// any VM start + listen latency.
	boundPort := waitForContainerPort(t, env, listenerCID, 15*time.Second)
	t.Logf("container echosrv bound on port %s", boundPort)

	// The container is inside the sandbox netns; its listener is reachable
	// at the veth address from inside that namespace. Dial from inside it.
	endpoint := net.JoinHostPort(vethAddr, boundPort)
	const token = "netns-inbound-ok"
	done := dialInNetnsRoundTrip(t, netns.path, endpoint, token)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dial from inside sandbox namespace to container listener failed: %v\n"+
				"(the shim must make the container's listener reachable from within "+
				"the provided network sandbox)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for in-netns dial to container listener to complete")
	}

	// echosrv exits after one exchange; wait for it.
	waitResp, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: listenerCID})
	if err != nil {
		t.Fatalf("Task.Wait listener: %v", err)
	}
	if waitResp.GetExitStatus() != 0 {
		t.Fatalf("listener container exit status: got %d, want 0", waitResp.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: listenerCID}) //nolint:errcheck

	t.Log("inbound connection from inside network sandbox to container listener: ok")
}
