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

// This file contains SandboxSuite tests that verify the shim API contract
// for member-container workloads: status fields, host-network sandboxes,
// exec, shared endpoints, and outbound networking.  Every test is framed
// in terms of the shim API specification, not any particular
// implementation.

package shimtest

import (
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// testStatusReportsPidAndCreatedAt verifies that SandboxStatus always returns
// a non-zero pid and a non-zero created_at timestamp.
//
// The API contract: a caller must be able to reference the sandbox's
// namespaces (e.g. /proc/<pid>/ns/*) and report the sandbox's age, so a
// shim must populate a non-zero pid and a non-zero created_at after a
// successful StartSandbox.
func (s *SandboxSuite) testStatusReportsPidAndCreatedAt(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	_, info := sandboxStatusInfo(t, env)

	resp, err := env.sc.SandboxStatus(env.ctx, &sandboxAPI.SandboxStatusRequest{
		SandboxID: sandboxID,
	})
	if err != nil {
		t.Fatalf("SandboxStatus: %v", err)
	}

	if resp.GetPid() == 0 {
		t.Error("SandboxStatus.Pid must be non-zero after StartSandbox")
	}
	ts := resp.GetCreatedAt()
	if ts == nil || ts.AsTime().IsZero() {
		t.Error("SandboxStatus.CreatedAt must be non-zero after StartSandbox")
	}
	if info["pid"] == "" || info["pid"] == "0" {
		t.Errorf("SandboxStatus.Info[pid] must be a non-zero pid string, got %q", info["pid"])
	}
	if info["state"] == "" {
		t.Errorf("SandboxStatus.Info[state] must not be empty, got %q", info["state"])
	}
	t.Logf("status ok: pid=%d created_at=%s info=%v", resp.GetPid(), resp.GetCreatedAt().AsTime(), info)
}

// testHostNetworkNoNetworkSandbox verifies that a sandbox created with an
// empty NetnsPath (i.e. no network sandbox provided) succeeds and that
// member containers run normally.
//
// The API contract: an empty netns_path in CreateSandboxRequest means the
// sandbox uses the host's network stack (no isolation).  The shim must accept
// this case without error and member containers must run successfully.
func (s *SandboxSuite) testHostNetworkNoNetworkSandbox(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShimWithNetworkSandbox(t, s.cfg, sandboxID, "")

	cid := createContainerInSandbox(t, env, []string{"/bin/echo", "host-network-ok"})
	readContainerOutput(t, env, cid, "host-network-ok", 30*time.Second)

	waitResp, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatalf("Task.Wait: %v", err)
	}
	if waitResp.GetExitStatus() != 0 {
		t.Errorf("container exit status: got %d, want 0", waitResp.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid}) //nolint:errcheck
	t.Log("host-network sandbox: member container ran successfully")
}

// testMemberContainerExec verifies that a process can be exec'd into a running
// member container and that its output and exit status are correctly propagated.
//
// The API contract: after Task.Create + Task.Start, a shim must accept
// Task.Exec to run an additional process inside the container.  The exec
// process must run inside the container's namespace and filesystem, its
// output must arrive on the configured stdio path, and Task.Wait on the
// ExecID must return the correct exit status.
//
// This contract underpins any exec-into-a-running-container use case
// (e.g. interactive exec, health/liveness probes, sidecar tooling).
func (s *SandboxSuite) testMemberContainerExec(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	// Start a long-lived container to exec into.
	observerCID := createContainerInSandbox(t, env, []string{"/bin/forever", "exec-target"})

	const token = "exec-probe-ok"
	out, exitStatus := execInSandboxContainer(t, env, observerCID, []string{"/bin/echo", token}, 30*time.Second)
	if !strings.Contains(out, token) {
		t.Errorf("exec output: want %q in output, got %q", token, out)
	}
	if exitStatus != 0 {
		t.Errorf("exec exit status: got %d, want 0", exitStatus)
	}

	// Verify non-zero exit status propagation.
	_, nonZeroStatus := execInSandboxContainer(t, env, observerCID, []string{"/bin/exit", "42"}, 30*time.Second)
	if nonZeroStatus != 42 {
		t.Errorf("exec exit-code propagation: got %d, want 42", nonZeroStatus)
	}

	env.tc.Kill(env.ctx, &taskAPI.KillRequest{ID: observerCID, Signal: uint32(syscall.SIGKILL), All: true}) //nolint:errcheck
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: observerCID})                                             //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: observerCID})                                         //nolint:errcheck
	t.Log("exec in sandbox member container: ok")
}

// testCrossContainerViaUDS verifies that two member containers in one sandbox
// can both reach a shared host-side UNIX domain socket endpoint by exec'ing
// into a container that has the socket forwarded into its filesystem.
//
// The API contract: a member container that has a "uds" mount type in its
// OCI spec must receive the corresponding host-side UNIX socket forwarded into
// its filesystem.  A process exec'd into the container must be able to connect
// to that socket.  This is the general contract behind any shared, pre-forwarded
// host endpoint: multiple processes in a sandbox must be able to reach the
// same endpoint through it.
//
// Test topology:
//
//	host UNIX socket listener (the shared endpoint)
//	  └── forwarded into the shared container at /run/shared.sock
//	      ├── exec A: nc -U /run/shared.sock  (connects, verified by host accept)
//	      └── exec B: nc -U /run/shared.sock  (connects, verified by host accept)
//
// Using exec into a single container avoids the per-container socket-forward
// routing ambiguity that arises when multiple containers each have their own
// accept stream.
func (s *SandboxSuite) testCrossContainerViaUDS(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	// Create a host-side UNIX socket listener (the shared endpoint).
	hostSockPath, err := makeUnixSockPath(t)
	if err != nil {
		t.Fatalf("create host sock path: %v", err)
	}
	ln, err := net.Listen("unix", hostSockPath)
	if err != nil {
		t.Fatalf("host unix listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	const containerSockPath = "/run/shared.sock"

	// Accept connections from the container and immediately close them.
	// Closing causes nc to see EOF on the socket and exit cleanly.
	hostDone := make(chan error, 2)
	acceptOne := func() {
		conn, err := ln.Accept()
		if err != nil {
			hostDone <- err
			return
		}
		conn.Close()
		hostDone <- nil
	}
	go acceptOne()
	go acceptOne()

	// Start a long-lived container with the host socket forwarded into it.
	sharedCID := createContainerInSandbox(t, env,
		[]string{"/bin/forever", "uds-shared-container"},
		withSandboxCtrExtraMounts(specs.Mount{
			Type:        "uds",
			Source:      hostSockPath,
			Destination: containerSockPath,
		}),
	)

	// Exec nc twice into the shared container; each connection proves that
	// a process running in the container can reach the shared host endpoint.
	// These model two different processes reaching a common host-forwarded
	// endpoint, as multiple containers in a sandbox would.
	for i := 0; i < 2; i++ {
		out, exitCode := execInSandboxContainer(
			t, env, sharedCID,
			[]string{"/bin/nc", "-U", containerSockPath},
			30*time.Second,
		)
		if exitCode != 0 {
			t.Errorf("nc exec %d: exit code %d, output: %q", i+1, exitCode, out)
		}
		// The host must have seen a connection for this exec.
		select {
		case err := <-hostDone:
			if err != nil {
				t.Fatalf("host accept connection %d: %v", i+1, err)
			}
			t.Logf("cross-container UDS connection %d: ok", i+1)
		case <-time.After(5 * time.Second):
			t.Fatalf("host did not see connection %d within 5s", i+1)
		}
	}

	// Kill the shared container.
	env.tc.Kill(env.ctx, &taskAPI.KillRequest{ID: sharedCID, Signal: uint32(syscall.SIGKILL), All: true}) //nolint:errcheck
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: sharedCID})                                             //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: sharedCID})                                         //nolint:errcheck
	t.Log("cross-container UDS: both execs reached the shared host endpoint")
}

// testContainerOutboundTCP verifies that a process running inside a member
// container has a working outbound network path by resolving a real
// external hostname.
//
// The API contract: a shim must give member containers a working network
// stack, regardless of the mechanism used to provide it (native network
// namespace membership, a virtual NIC, or any other in-guest networking
// approach). This test does not care how connectivity is achieved — only
// that a container can reach a resolver and get back a valid answer,
// exactly as any container workload that depends on DNS would.
//
// DNS resolution (rather than a raw TCP round trip) is used here because
// Task.Exec — the mechanism this suite uses to run a process inside an
// already-running member container — has no stdin plumbing, and a TCP
// round trip needs a way to send data. /bin/host takes its input purely
// from argv and writes its result to stdout, so it fits Task.Exec's
// existing capabilities. NetworkSuite (legacy path) separately covers TCP
// and UDP round trips end-to-end.
func (s *SandboxSuite) testContainerOutboundTCP(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	cid := createContainerInSandbox(t, env, []string{"/bin/forever", "outbound-container"})

	// host <name>: prints "<name> has address <ip>" for each resolved address.
	out, exitStatus := execInSandboxContainer(t, env, cid, []string{"/bin/host", dnsTestHostname}, 30*time.Second)
	if exitStatus != 0 {
		t.Fatalf("host exec exit status: got %d, want 0; output: %q", exitStatus, out)
	}

	var addrs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const marker = " has address "
		idx := strings.Index(line, marker)
		if idx < 0 {
			t.Errorf("host %s: unexpected output line %q", dnsTestHostname, line)
			continue
		}
		ip := line[idx+len(marker):]
		if net.ParseIP(ip) == nil {
			t.Errorf("host %s: %q is not a valid IP address", dnsTestHostname, ip)
			continue
		}
		addrs = append(addrs, ip)
	}
	if len(addrs) == 0 {
		t.Fatalf("host %s produced no addresses; output: %q", dnsTestHostname, out)
	}

	t.Log("container outbound DNS resolution: ok, addresses:", addrs)
}

// makeUnixSockPath returns a UNIX socket path under a temp directory that
// satisfies the 104-byte AF_UNIX path limit on macOS.
func makeUnixSockPath(tb testing.TB) (string, error) {
	tb.Helper()
	dir, err := os.MkdirTemp(unixSafeDir(), "nb-uds-")
	if err != nil {
		return "", err
	}
	tb.Cleanup(func() { os.RemoveAll(dir) })
	return dir + "/shared.sock", nil
}
