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
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
)

// sandboxStateReady and sandboxStateNotReady are the two values a shim's
// SandboxStatusResponse.State must use.
//
// The runtime/sandbox/v1 proto itself only documents State as a plain
// string, with no enumerated values on the wire. But an unconstrained
// free-form string is not a usable API contract: any caller that needs to
// branch on sandbox readiness has to match against *some* fixed vocabulary,
// and a shim that invents its own spelling (a plausible one like "ready"
// included) breaks every caller that expects the specified names. State is
// therefore specified here as exactly one of these two strings, not an
// arbitrary human-readable state name. This is exactly the kind of
// externally-observable contract shimtest exists to check: it is invisible
// in the base shim-v2 sandbox protocol's type signature, but load-bearing
// for any caller that inspects sandbox readiness — for example,
// containerd's CRI layer maps these exact names onto the CRI v1
// PodSandboxState enum when the shim sandboxer is configured.
const (
	sandboxStateReady    = "SANDBOX_READY"
	sandboxStateNotReady = "SANDBOX_NOTREADY"
)

// SandboxSuite verifies the containerd sandbox shim API contract
// (runtime/sandbox/v1).  Tests in this suite cover:
//
//   - Sandbox lifecycle (create → start → status → stop → shutdown)
//   - Platform and Ping RPCs
//   - Member container routing: tasks created on the shared connection
//     after StartSandbox must run correctly inside the sandbox
//   - Multiple concurrent containers sharing one sandbox
//   - Per-container Delete independence (does not tear down the sandbox)
//   - WaitSandbox unblocks on stop
//   - Protocol error cases (duplicate Create, Start before Create)
//   - Resource release: no mount leaks after shutdown
//
// The suite is gated on the "sandbox" feature key; it is never skipped
// once enabled — every failure is a conformance failure.
type SandboxSuite struct {
	cfg Config
}

// NewSandboxSuite constructs a SandboxSuite from cfg.
func NewSandboxSuite(cfg Config) *SandboxSuite {
	return &SandboxSuite{cfg: cfg}
}

// Run runs every test in the suite as a subtest of t.
func (s *SandboxSuite) Run(t *testing.T) {
	t.Helper()
	registerShimLeakCheck(t, s.cfg.ShimBinary)

	t.Run("Lifecycle", s.testLifecycle)
	t.Run("Platform", s.testPlatform)
	t.Run("Ping", s.testPing)
	t.Run("SingleContainer", s.testSingleContainer)
	t.Run("MultipleContainers", s.testMultipleContainers)
	t.Run("ContainerLifecycleIndependence", s.testContainerLifecycleIndependence)
	t.Run("StatusAfterStop", s.testStatusAfterStop)
	t.Run("WaitUnblocksOnStop", s.testWaitUnblocksOnStop)
	t.Run("CreateTwiceRejected", s.testCreateTwiceRejected)
	t.Run("StartWithoutCreateRejected", s.testStartWithoutCreateRejected)
	t.Run("ResourceReleaseOnShutdown", s.testResourceReleaseOnShutdown)

	// Member-container workload contracts (exec, shared namespaces,
	// volumes, networking).
	t.Run("StatusReportsPidAndCreatedAt", s.testStatusReportsPidAndCreatedAt)
	t.Run("HostNetworkNoNetworkSandbox", s.testHostNetworkNoNetworkSandbox)
	t.Run("MemberContainerExec", s.testMemberContainerExec)
	t.Run("CrossContainerViaUDS", s.testCrossContainerViaUDS)
	t.Run("NetworkSandboxHeldOpen", s.testNetworkSandboxHeldOpen)
	t.Run("NetworkSandboxPathInStatus", s.testNetworkSandboxPathInStatus)
	t.Run("ContainerOutboundTCP", s.testContainerOutboundTCP)
	t.Run("ContainerTrafficScopedToNetworkSandbox", s.testContainerTrafficScopedToNetworkSandbox)
	t.Run("MemberContainersShareNetwork", s.testMemberContainersShareNetwork)
	t.Run("MemberContainerHostVolume", s.testMemberContainerHostVolume)
	t.Run("MemberContainersSharePID", s.testMemberContainersSharePID)
	t.Run("MemberContainersShareIPC", s.testMemberContainersShareIPC)
}

// testLifecycle drives the sandbox through the full lifecycle:
//
//	CreateSandbox → StartSandbox → SandboxStatus(ready) →
//	StopSandbox → SandboxStatus(stopped) → ShutdownSandbox
//
// The shim must transition through the expected states and the
// ShutdownSandbox RPC must succeed.
func (s *SandboxSuite) testLifecycle(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	// After StartSandbox the status must reflect a running sandbox.
	status, err := env.sc.SandboxStatus(env.ctx, &sandboxAPI.SandboxStatusRequest{
		SandboxID: sandboxID,
	})
	if err != nil {
		t.Fatalf("SandboxStatus: %v", err)
	}
	if status.GetState() != sandboxStateReady {
		t.Errorf("SandboxStatus state after start: got %q, want %q", status.GetState(), sandboxStateReady)
	}
	if status.GetPid() == 0 {
		t.Error("SandboxStatus Pid must be > 0 after start")
	}
	t.Logf("sandbox running: state=%s pid=%d", status.GetState(), status.GetPid())

	// StopSandbox must succeed and transition state to stopped.
	if _, err := env.sc.StopSandbox(env.ctx, &sandboxAPI.StopSandboxRequest{
		SandboxID: sandboxID,
	}); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}

	// Poll status: the shim must report SANDBOX_NOTREADY after stop.
	if err := waitForSandboxStatus(env.ctx, env.sc, sandboxID, sandboxStateNotReady, 10*time.Second); err != nil {
		t.Errorf("SandboxStatus state after stop: %v", err)
	}

	// ShutdownSandbox must succeed even though the sandbox is already stopped.
	if _, err := env.sc.ShutdownSandbox(env.ctx, &sandboxAPI.ShutdownSandboxRequest{
		SandboxID: sandboxID,
	}); err != nil {
		t.Fatalf("ShutdownSandbox: %v", err)
	}

	t.Log("sandbox lifecycle complete")
}

// testPlatform verifies that Platform returns a valid OS/architecture.
// The shim must always honour this RPC; containerd uses it to generate
// a correct OCI spec for member containers.
func (s *SandboxSuite) testPlatform(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	resp, err := env.sc.Platform(env.ctx, &sandboxAPI.PlatformRequest{SandboxID: sandboxID})
	if err != nil {
		t.Fatalf("Platform: %v", err)
	}
	p := resp.GetPlatform()
	if p == nil {
		t.Fatal("Platform response missing platform field")
	}
	if p.GetOS() == "" {
		t.Error("Platform response: OS must not be empty")
	}
	if p.GetArchitecture() == "" {
		t.Error("Platform response: Architecture must not be empty")
	}
	t.Logf("platform: os=%s arch=%s variant=%s", p.GetOS(), p.GetArchitecture(), p.GetVariant())
}

// testPing verifies that PingSandbox succeeds while the sandbox is
// running.  PingSandbox is a lightweight liveness check; it must
// return without error from a live sandbox.
func (s *SandboxSuite) testPing(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	if _, err := env.sc.PingSandbox(env.ctx, &sandboxAPI.PingRequest{SandboxID: sandboxID}); err != nil {
		t.Fatalf("PingSandbox: %v", err)
	}
	t.Log("PingSandbox succeeded")
}

// testSingleContainer verifies that a single member container can be
// created, started, and produce output inside the sandbox.
//
// The API contract: after StartSandbox, Task.Create on the shared
// connection must create a container that runs inside the sandbox.
// Task.Start must make the container's init process runnable, and
// Task.Wait must return the init process exit status.
func (s *SandboxSuite) testSingleContainer(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	cid := createContainerInSandbox(t, env, []string{"/bin/echo", "hello-sandbox"})

	// Read output — the container must produce the expected string.
	readContainerOutput(t, env, cid, "hello-sandbox", 30*time.Second)

	// Wait for the container's natural exit and verify exit status.
	waitResp, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatalf("Task.Wait: %v", err)
	}
	if waitResp.GetExitStatus() != 0 {
		t.Errorf("expected exit status 0, got %d", waitResp.GetExitStatus())
	}

	// Delete the container task.
	if _, err := env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid}); err != nil {
		t.Fatalf("Task.Delete: %v", err)
	}
	t.Log("single container complete")
}

// testMultipleContainers verifies that three member containers can run
// concurrently inside one sandbox, each receiving its own isolated
// rootfs and producing the expected output.
//
// The API contract: the sandbox must support N concurrent member
// containers.  Each container's output must be independent; the sandbox
// VM must not be torn down when one container exits.
func (s *SandboxSuite) testMultipleContainers(t *testing.T) {
	const n = 3
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	type ctrResult struct {
		cid   string
		token string
	}

	// Create all containers first, then collect output concurrently.
	ctrs := make([]ctrResult, n)
	for i := range ctrs {
		token := fmt.Sprintf("ctr%d-token-%s", i, randomSuffix())
		cid := createContainerInSandbox(t, env, []string{"/bin/echo", token})
		ctrs[i] = ctrResult{cid: cid, token: token}
	}

	// Verify each container's output in parallel.
	var wg sync.WaitGroup
	for _, cr := range ctrs {
		wg.Add(1)
		go func(cr ctrResult) {
			defer wg.Done()
			readContainerOutput(t, env, cr.cid, cr.token, 30*time.Second)
		}(cr)
	}
	wg.Wait()

	// All containers should have exited cleanly by now.
	for _, cr := range ctrs {
		waitResp, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cr.cid})
		if err != nil {
			t.Errorf("Task.Wait %s: %v", cr.cid, err)
			continue
		}
		if waitResp.GetExitStatus() != 0 {
			t.Errorf("container %s exit status: got %d, want 0", cr.cid, waitResp.GetExitStatus())
		}
		env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cr.cid}) //nolint:errcheck
	}

	t.Logf("all %d containers completed", n)
}

// testContainerLifecycleIndependence verifies that deleting one member
// container leaves the sandbox and other containers running.
//
// The API contract: Task.Delete for a member container must clean up
// that container's resources without affecting the sandbox VM or any
// other member containers.
func (s *SandboxSuite) testContainerLifecycleIndependence(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	// Start a long-lived "observer" container.
	observerCID := createContainerInSandbox(t, env, []string{"/bin/forever", "observer"})

	// Start a short-lived container that exits naturally.
	shortCID := createContainerInSandbox(t, env, []string{"/bin/echo", "short-lived"})
	readContainerOutput(t, env, shortCID, "short-lived", 30*time.Second)

	// Wait for the short container to exit and delete it.
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: shortCID}) //nolint:errcheck
	if _, err := env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: shortCID}); err != nil {
		t.Fatalf("Delete short container: %v", err)
	}

	// The observer container must still be running.
	stateResp, err := env.tc.State(env.ctx, &taskAPI.StateRequest{ID: observerCID})
	if err != nil {
		t.Fatalf("State for observer after short-container delete: %v", err)
	}
	if stateResp.GetStatus() != tasktypes.Status_RUNNING {
		t.Errorf("observer container status after peer delete: got %v, want RUNNING", stateResp.GetStatus())
	}

	// The sandbox itself must also still be running.
	status, err := env.sc.SandboxStatus(env.ctx, &sandboxAPI.SandboxStatusRequest{SandboxID: sandboxID})
	if err != nil {
		t.Fatalf("SandboxStatus after peer delete: %v", err)
	}
	if status.GetState() != sandboxStateReady {
		t.Errorf("sandbox state after peer-container delete: got %q, want %q", status.GetState(), sandboxStateReady)
	}

	t.Log("observer still running after peer delete; sandbox intact")

	// Clean up the observer.
	env.tc.Kill(env.ctx, &taskAPI.KillRequest{ID: observerCID, Signal: uint32(syscall.SIGKILL), All: true}) //nolint:errcheck
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: observerCID})                                             //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: observerCID})                                         //nolint:errcheck
}

// testStatusAfterStop verifies that SandboxStatus after StopSandbox
// reports a non-ready state, and that a second StopSandbox is
// idempotent (must not error).
func (s *SandboxSuite) testStatusAfterStop(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	// First stop.
	if _, err := env.sc.StopSandbox(env.ctx, &sandboxAPI.StopSandboxRequest{SandboxID: sandboxID}); err != nil {
		t.Fatalf("StopSandbox (first): %v", err)
	}

	// Status must not be sandboxStateReady after stop.
	status, err := env.sc.SandboxStatus(env.ctx, &sandboxAPI.SandboxStatusRequest{SandboxID: sandboxID})
	if err != nil {
		t.Logf("SandboxStatus after stop returned error (may be acceptable): %v", err)
	} else if status.GetState() == sandboxStateReady {
		t.Errorf("SandboxStatus after stop: state is still %q; shim must not report ready after stop", status.GetState())
	}

	// Second stop must be idempotent — must not return an error.
	if _, err := env.sc.StopSandbox(env.ctx, &sandboxAPI.StopSandboxRequest{SandboxID: sandboxID}); err != nil {
		t.Errorf("StopSandbox (second, idempotency check): %v", err)
	}

	t.Log("status-after-stop and idempotency checks passed")
}

// testWaitUnblocksOnStop verifies that WaitSandbox returns after the
// sandbox is stopped.
//
// The API contract: WaitSandbox must unblock when the sandbox exits
// (via StopSandbox or ShutdownSandbox).  Callers rely on this to
// detect sandbox death.
func (s *SandboxSuite) testWaitUnblocksOnStop(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	waitDone := make(chan error, 1)
	go func() {
		_, err := env.sc.WaitSandbox(env.ctx, &sandboxAPI.WaitSandboxRequest{SandboxID: sandboxID})
		waitDone <- err
	}()

	// Give WaitSandbox a moment to start blocking.
	time.Sleep(200 * time.Millisecond)

	if _, err := env.sc.StopSandbox(env.ctx, &sandboxAPI.StopSandboxRequest{SandboxID: sandboxID}); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}

	select {
	case err := <-waitDone:
		if err != nil {
			t.Logf("WaitSandbox returned error after stop (may be acceptable for ttrpc shutdown): %v", err)
		} else {
			t.Log("WaitSandbox returned cleanly after stop")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("WaitSandbox did not return within 15s after StopSandbox")
	}
}

// testCreateTwiceRejected verifies that calling CreateSandbox a second
// time on the same shim returns an AlreadyExists error.
//
// The API contract: a sandbox shim process hosts exactly one sandbox.
// A second CreateSandbox must be rejected with AlreadyExists.
func (s *SandboxSuite) testCreateTwiceRejected(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	_, err := env.sc.CreateSandbox(env.ctx, &sandboxAPI.CreateSandboxRequest{
		SandboxID:  sandboxID + "-dup",
		BundlePath: ".",
	})
	if err == nil {
		t.Fatal("second CreateSandbox must fail; got nil error")
	}
	errStr := strings.ToLower(err.Error())
	if !strings.Contains(errStr, "already exists") && !strings.Contains(errStr, "alreadyexists") {
		t.Errorf("second CreateSandbox: expected AlreadyExists error, got: %v", err)
	}
	t.Logf("second CreateSandbox correctly rejected: %v", err)
}

// testStartWithoutCreateRejected verifies that StartSandbox before
// CreateSandbox fails with FailedPrecondition.
//
// The API contract: CreateSandbox must precede StartSandbox.  The shim
// must reject StartSandbox if CreateSandbox has not been called first.
func (s *SandboxSuite) testStartWithoutCreateRejected(t *testing.T) {
	shimBin, bundleDir, _ := shimSetup(t, s.cfg)
	sandboxID := containerID(t)
	ns := uniqueTestNamespace(t, "sandbox")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	// The shim reads config.json from its working directory for the
	// grouping label.  Write a minimal sandbox spec so the shim can start.
	writeSandboxOCISpec(t, bundleDir)

	// Start a fresh shim without calling CreateSandbox.
	params := startShim(t, shimBin, bundleDir, sandboxID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	sc := sandboxAPI.NewTTRPCSandboxClient(client)
	tc := taskAPI.NewTTRPCTaskClient(client)

	_, err := sc.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{SandboxID: sandboxID})
	if err == nil {
		t.Fatal("StartSandbox without CreateSandbox must fail; got nil error")
	}
	errStr := strings.ToLower(err.Error())
	if !strings.Contains(errStr, "precondition") && !strings.Contains(errStr, "failed_precondition") {
		t.Errorf("StartSandbox without create: expected FailedPrecondition, got: %v", err)
	}
	t.Logf("StartSandbox before CreateSandbox correctly rejected: %v", err)

	// Shut the shim down cleanly.
	shutdownTask(ctx, tc, sandboxID)
}

// testResourceReleaseOnShutdown verifies that ShutdownSandbox releases
// per-container host resources.  Specifically, if the shim uses a
// shared virtiofs directory, paths under that directory must not appear
// as mount points in the shim's mount namespace after shutdown.
//
// The API contract: ShutdownSandbox must release all resources
// allocated for the sandbox and its member containers.  Leaked mount
// points can prevent bundle-directory cleanup and exhaust kernel mount
// table entries.
func (s *SandboxSuite) testResourceReleaseOnShutdown(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	// Run two member containers to create per-container rootfs mounts.
	cid1 := createContainerInSandbox(t, env, []string{"/bin/echo", "ctr1"})
	cid2 := createContainerInSandbox(t, env, []string{"/bin/echo", "ctr2"})

	readContainerOutput(t, env, cid1, "ctr1", 30*time.Second)
	readContainerOutput(t, env, cid2, "ctr2", 30*time.Second)

	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid1})     //nolint:errcheck
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid2})     //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid1}) //nolint:errcheck
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid2}) //nolint:errcheck

	// Capture the shim PID before shutdown so we can inspect its
	// namespace after.  Use a probe container-ID; Connect returns the
	// shim PID regardless of which ID is used on some shims.
	shimPID := sandboxShimPID(env, cid1)
	mountsBefore := sandboxContainersMounts(shimPID)
	t.Logf("shim PID: %d, container mounts before shutdown: %v", shimPID, mountsBefore)

	// Stop and shut down the sandbox.
	env.sc.StopSandbox(env.ctx, &sandboxAPI.StopSandboxRequest{SandboxID: sandboxID})         //nolint:errcheck
	env.sc.ShutdownSandbox(env.ctx, &sandboxAPI.ShutdownSandboxRequest{SandboxID: sandboxID}) //nolint:errcheck

	// Give the shim time to clean up.
	time.Sleep(500 * time.Millisecond)

	// After shutdown, no per-container mounts should remain in the
	// shim's namespace.  We check the shim's /proc/<pid>/mountinfo
	// if the shim runs in a private mount namespace; if the shim
	// exited (pid gone) that is also a clean result.
	mountsAfter := sandboxContainersMounts(shimPID)
	if len(mountsAfter) > 0 {
		t.Errorf("shim left %d per-container mount(s) after ShutdownSandbox: %v",
			len(mountsAfter), mountsAfter)
	} else {
		t.Log("no per-container mounts remain after shutdown")
	}
}

// Ensure ttrpc import is used (consumed in testStartWithoutCreateRejected).
var _ *ttrpc.Client
