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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
)

// containerOutput holds the captured stdout for a member container.
type containerOutput struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

// sandboxEnv holds the shared state for a running sandbox: the TTRPC
// client, sandbox and task service clients, and the list of member
// containers created so far.
type sandboxEnv struct {
	ctx       context.Context
	client    *ttrpc.Client
	sc        sandboxAPI.TTRPCSandboxService
	tc        taskAPI.TTRPCTaskService
	sandboxID string
	address   string

	mu         sync.Mutex
	containers []string                    // member container IDs in creation order
	stdoutBufs map[string]*containerOutput // cid -> captured stdout
	stdinPaths map[string]string           // cid -> stdin FIFO path (only set when withSandboxCtrStdin is used)
}

// startSandboxShim starts the shim binary for a sandbox and drives it
// through CreateSandbox + StartSandbox.  It returns a *sandboxEnv
// backed by the shared TTRPC connection.
//
// API contract enforced here:
//   - Bootstrap version ≥ 3 (enables per-connection task routing).
//   - CreateSandbox and StartSandbox must succeed.
//   - StartSandboxResponse.Pid must be > 0.
//   - StartSandboxResponse.CreatedAt must be set and non-zero.
//
// Cleanup (ShutdownSandbox + shim delete) is registered on tb.
// startSandboxShim starts a sandbox shim with no network sandbox (host-network
// or platforms where network sandboxes are not supported).
// Use startSandboxShimWithNetworkSandbox to pass a network sandbox path.
func startSandboxShim(tb testing.TB, cfg Config, sandboxID string) *sandboxEnv {
	tb.Helper()
	return startSandboxShimInner(tb, cfg, sandboxID, "")
}

// writeSandboxOCISpec writes a minimal OCI config.json suitable for a
// pod-sandbox bundle.  The spec carries resource annotations so that
// VM-based shims start with a small VM; shims that do not use these
// annotations ignore them.
func writeSandboxOCISpec(tb testing.TB, bundleDir string) {
	tb.Helper()
	spec := struct {
		OciVersion  string            `json:"ociVersion"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}{
		OciVersion: "1.0.2",
		Annotations: map[string]string{
			"io.containerd.nerdbox.resources.cpu":    "2",
			"io.containerd.nerdbox.resources.memory": "2048",
		},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		tb.Fatal("marshal sandbox OCI spec:", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), data, 0o644); err != nil {
		tb.Fatal("write sandbox config.json:", err)
	}
}

// shutdownSandboxShim is the cleanup function registered by
// startSandboxShim.  It tears down any remaining member containers
// then calls StopSandbox and ShutdownSandbox.  Errors are logged but
// not fatal so that cleanup proceeds even after a failed test.
func shutdownSandboxShim(tb testing.TB, env *sandboxEnv) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
	defer cancel()

	env.mu.Lock()
	ctrs := make([]string, len(env.containers))
	copy(ctrs, env.containers)
	env.mu.Unlock()

	for _, cid := range ctrs {
		env.tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: 9, All: true}) //nolint:errcheck
		env.tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})                       //nolint:errcheck
		env.tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})                   //nolint:errcheck
	}

	env.sc.StopSandbox(ctx, &sandboxAPI.StopSandboxRequest{SandboxID: env.sandboxID})         //nolint:errcheck
	env.sc.ShutdownSandbox(ctx, &sandboxAPI.ShutdownSandboxRequest{SandboxID: env.sandboxID}) //nolint:errcheck
}

// createContainerInSandbox creates (and by default starts) a member
// container inside an already-running sandbox.  It uses the same
// TTRPC connection that was used for the sandbox lifecycle RPCs, which
// is the routing mechanism containerd uses in production.
//
// Stdout is captured into an internal buffer; callers read it via
// readContainerOutput(env, cid).  Cleanup (kill/wait/delete) is
// registered on tb.
//
// Returns the container ID.
func createContainerInSandbox(tb testing.TB, env *sandboxEnv, args []string, specOpts ...func(*sandboxCtrSpec)) string {
	tb.Helper()

	so := &sandboxCtrSpec{}
	for _, opt := range specOpts {
		opt(so)
	}

	cid := containerID(tb)

	bundleDir := tb.TempDir()
	bundleDir, err := filepath.EvalSymlinks(bundleDir)
	if err != nil {
		tb.Fatal("evalSymlinks member bundleDir:", err)
	}

	// Member containers: build the rootfs from the embedded testbin.
	// For the sandbox path, ShareRootfs on the host will assemble the
	// rootfs from whatever mounts are provided.  We must give the shim
	// a mount spec it can execute on the host.
	//
	// When running as root, use FormatMounts=true (erofs images with an
	// overlay descriptor) so the shim assembles the overlay properly.
	//
	// When running as non-root, FormatMounts=false extracts the rootfs
	// directly into bundleDir/rootfs and returns nil mounts.  In that
	// case we provide a single bind mount of that pre-extracted dir so
	// ShareRootfs can bind it into the shared dir.
	cfg := Config{FormatMounts: os.Getuid() == 0}
	rootfsMounts := buildEmbeddedRootfs(tb, bundleDir, cfg)

	// Non-root / pre-extracted path: nil mounts means the rootfs is
	// already in bundleDir/rootfs — present it as a bind mount.
	if len(rootfsMounts) == 0 {
		rootfsMounts = []*types.Mount{{
			Type:    "bind",
			Source:  filepath.Join(bundleDir, "rootfs"),
			Options: []string{"ro", "rbind"},
		}}
	}

	var ociOpts []func(*specs.Spec)
	if len(so.extraMounts) > 0 {
		ociOpts = append(ociOpts, withExtraMounts(so.extraMounts...))
	}
	ociOpts = append(ociOpts, so.ociOpts...)
	createOCISpec(tb, bundleDir, args, cfg, ociOpts...)

	var stdinPath, stdoutPath, stderrPath string
	if so.stdin {
		stdinPath, stdoutPath, stderrPath = createStdioFifos(tb, bundleDir)
	} else {
		stdoutPath, stderrPath = createIOFifos(tb, bundleDir)
	}
	// Start capturing stdout into a buffer before Task.Create so the
	// shim's forwardIO can open the write end without blocking.
	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	drainFifoInto(tb, env.ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	// Stderr is discarded.
	drainFifo(tb, env.ctx, stderrPath)

	var req *taskAPI.CreateTaskRequest
	if so.stdin {
		req = newCreateTaskRequestStdin(tb, cid, bundleDir, stdinPath, stdoutPath, stderrPath, rootfsMounts)
	} else {
		req = newCreateTaskRequest(tb, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)
	}
	if _, err := env.tc.Create(env.ctx, req); err != nil {
		tb.Fatalf("Task.Create member %s: %v", cid, err)
	}

	if !so.noStart {
		if _, err := env.tc.Start(env.ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
			tb.Fatalf("Task.Start member %s: %v", cid, err)
		}
	}

	env.mu.Lock()
	env.containers = append(env.containers, cid)
	env.stdoutBufs[cid] = &containerOutput{buf: &stdoutBuf, mu: &stdoutMu}
	if so.stdin {
		if env.stdinPaths == nil {
			env.stdinPaths = make(map[string]string)
		}
		env.stdinPaths[cid] = stdinPath
	}
	env.mu.Unlock()

	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
		defer cancel()
		env.tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: 9, All: true}) //nolint:errcheck
		env.tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})                       //nolint:errcheck
		env.tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})                   //nolint:errcheck

		env.mu.Lock()
		for i, id := range env.containers {
			if id == cid {
				env.containers = append(env.containers[:i], env.containers[i+1:]...)
				break
			}
		}
		delete(env.stdoutBufs, cid)
		env.mu.Unlock()
	})

	return cid
}

// sandboxCtrSpec carries options for createContainerInSandbox.
type sandboxCtrSpec struct {
	noStart     bool
	stdin       bool
	extraMounts []specs.Mount       // extra OCI mounts appended to the container spec
	ociOpts     []func(*specs.Spec) // extra low-level OCI spec opts (e.g. namespace sharing)
}

// withSandboxCtrStdin requests that createContainerInSandbox wire up a
// stdin FIFO for the member container, in addition to stdout/stderr. Use
// writeContainerStdin to send data once the container is running.
func withSandboxCtrStdin() func(*sandboxCtrSpec) {
	return func(o *sandboxCtrSpec) { o.stdin = true }
}

// withSandboxCtrExtraMounts appends mounts to the container's OCI spec.
// Use this to inject shared volumes, /dev/shm bind-mounts, or UDS-mount
// entries into a sandbox member container.
func withSandboxCtrExtraMounts(mounts ...specs.Mount) func(*sandboxCtrSpec) {
	return func(o *sandboxCtrSpec) {
		o.extraMounts = append(o.extraMounts, mounts...)
	}
}

// withSandboxCtrNamespace requests that the given namespace type be set
// to a (placeholder) host path in the container's OCI spec, signaling
// that the container should join a namespace shared with its sandbox
// peers rather than a fresh, isolated one. See withHostPathNamespace for
// why the specific path value does not matter.
func withSandboxCtrNamespace(nsType specs.LinuxNamespaceType, path string) func(*sandboxCtrSpec) {
	return func(o *sandboxCtrSpec) {
		o.ociOpts = append(o.ociOpts, withHostPathNamespace(nsType, path))
	}
}

// createSandboxContainerFast creates and starts a member container using
// pre-built rootfs images.  It is the stress-loop counterpart of
// createContainerInSandbox: it avoids calling writeRootfsErofs /
// writeBigFileErofs on every iteration (which would exhaust tmpfs over
// thousands of iterations) by accepting images built once before the loop.
//
// preExtractedRootfs is a pre-populated directory used as the bind-mount
// source on non-root systems (where loop mounts are unavailable).  Pass ""
// on root systems where FormatMounts is true (the erofs+overlay path is used
// instead).
//
// Unlike createContainerInSandbox it does NOT register a tb.Cleanup, and it
// does NOT capture stdout into a buffer.  Callers must call
// releaseSandboxContainer when done with each container.  Stdout/stderr are
// drained and discarded.
func createSandboxContainerFast(ctx context.Context, tb testing.TB, env *sandboxEnv, cfg Config, imgs shimImages, preExtractedRootfs string, args []string) (string, error) {
	cid := containerID(tb)

	bundleDir := tb.TempDir()
	bundleDir, err := filepath.EvalSymlinks(bundleDir)
	if err != nil {
		return "", fmt.Errorf("evalSymlinks: %w", err)
	}

	rootfsDir := filepath.Join(bundleDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir rootfs: %w", err)
	}

	rootfsMounts := buildSandboxMemberMountsFromImages(tb, cfg, imgs, rootfsDir, preExtractedRootfs)

	createOCISpec(tb, bundleDir, args, Config{FormatMounts: cfg.FormatMounts})

	// Use null (empty) IO paths so the shim skips FIFO creation entirely.
	// This avoids the per-container FIFO files and the goroutines that drain
	// them from accumulating in the test's temp directory over thousands of
	// iterations.  The shim treats empty Stdout/Stderr as "discard IO".
	req := newCreateTaskRequest(tb, cid, bundleDir, "", "", rootfsMounts)
	if _, err := env.tc.Create(ctx, req); err != nil {
		return "", fmt.Errorf("Task.Create: %w", err)
	}
	if _, err := env.tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		return "", fmt.Errorf("Task.Start: %w", err)
	}

	env.mu.Lock()
	env.containers = append(env.containers, cid)
	env.mu.Unlock()

	return cid, nil
}

// buildSandboxMemberMountsFromImages builds rootfs mount specs for a stress
// iteration using pre-built images.  It only creates the per-iteration
// writable parts (ext4 scratch or overlay upper/work), reusing the read-only
// erofs images across iterations to avoid O(N) disk consumption.
//
// When running as root with FormatMounts, the full erofs+ext4+overlay path
// is used (same as benchContainerCreate).  Otherwise a bind mount of the
// given preExtractedRootfs directory is returned so ShareRootfs can copy it
// into the sandbox shared dir without needing loop devices.
// preExtractedRootfs must be pre-populated by the caller once before the
// loop; it is read-only and reused across all iterations.
func buildSandboxMemberMountsFromImages(tb testing.TB, cfg Config, imgs shimImages, rootfsDir, preExtractedRootfs string) []*types.Mount {
	tb.Helper()
	if cfg.FormatMounts && os.Getuid() == 0 {
		// Root + format mounts: use the erofs+ext4+overlay path.
		// rootfsDir gets a fresh ext4 scratch on each iteration.
		return buildRootfsMountsFromImages(tb, cfg, imgs, rootfsDir)
	}
	// Non-root or no format mounts: point at the pre-extracted directory.
	// ShareRootfs will copy it into the sandbox shared dir per container.
	if preExtractedRootfs == "" {
		// Fallback if caller did not pre-extract (shouldn't happen).
		extractErofsIntoDir(tb, imgs.erofsImg, rootfsDir)
		preExtractedRootfs = rootfsDir
	}
	return []*types.Mount{{
		Type:    "bind",
		Source:  preExtractedRootfs,
		Options: []string{"ro", "rbind"},
	}}
}

// releaseSandboxContainer immediately releases a container that was created
// with createContainerInSandbox.  It issues Task.Delete on the shim (which
// triggers host-side rootfs cleanup via Unshare) and removes the container
// from the env tracking maps so the memory is reclaimed during the run.
//
// This is the per-iteration counterpart to the tb.Cleanup registered by
// createContainerInSandbox.  Call it in stress loops where containers are
// short-lived: it prevents env.stdoutBufs from growing unboundedly across
// thousands of iterations and avoids stacking O(N) redundant tb.Cleanup
// registrations that would fire at test teardown.
//
// After releaseSandboxContainer returns the tb.Cleanup registered at
// creation time will still fire, but it becomes a no-op: the container is
// gone from env.containers and env.stdoutBufs, so the Kill/Wait/Delete RPCs
// will return NotFound and the map deletes are idempotent.
func releaseSandboxContainer(ctx context.Context, env *sandboxEnv, cid string) error {
	_, err := env.tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})

	env.mu.Lock()
	for i, id := range env.containers {
		if id == cid {
			env.containers = append(env.containers[:i], env.containers[i+1:]...)
			break
		}
	}
	delete(env.stdoutBufs, cid)
	env.mu.Unlock()

	return err
}

// withSandboxCtrNoStart creates the task without issuing Task.Start.
func withSandboxCtrNoStart() func(*sandboxCtrSpec) {
	return func(o *sandboxCtrSpec) { o.noStart = true }
}

// readContainerOutput waits up to timeout for want to appear in the
// captured stdout for the container with the given ID.  The container
// must have been created via createContainerInSandbox on env.
func readContainerOutput(tb testing.TB, env *sandboxEnv, cid, want string, timeout time.Duration) string {
	tb.Helper()
	env.mu.Lock()
	co := env.stdoutBufs[cid]
	env.mu.Unlock()
	if co == nil {
		tb.Fatalf("no captured stdout for container %s", cid)
	}
	deadline := time.After(timeout)
	for {
		co.mu.Lock()
		got := co.buf.String()
		co.mu.Unlock()
		if strings.Contains(got, want) {
			return got
		}
		select {
		case <-deadline:
			co.mu.Lock()
			final := co.buf.String()
			co.mu.Unlock()
			tb.Fatalf("timed out waiting for %q in stdout of %s, got: %q", want, cid, final)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// writeContainerStdin writes data to the stdin FIFO of a member container
// created with withSandboxCtrStdin, then closes the write end. The
// container must have been created via createContainerInSandbox with the
// withSandboxCtrStdin option.
func writeContainerStdin(tb testing.TB, env *sandboxEnv, cid, data string) {
	tb.Helper()
	env.mu.Lock()
	stdinPath := env.stdinPaths[cid]
	env.mu.Unlock()
	if stdinPath == "" {
		tb.Fatalf("no stdin FIFO for container %s (was it created with withSandboxCtrStdin?)", cid)
	}
	w, err := openPipeWriter(env.ctx, stdinPath)
	if err != nil {
		tb.Fatalf("open stdin fifo for %s: %v", cid, err)
	}
	if _, err := w.Write([]byte(data)); err != nil {
		tb.Fatalf("write stdin for %s: %v", cid, err)
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("close stdin fifo for %s: %v", cid, err)
	}
}

// readSandboxOutput waits up to timeout for want to appear in the FIFO
// at stdoutPath, returning the full accumulated output.  Prefer
// readContainerOutput when the container was created with
// createContainerInSandbox.
func readSandboxOutput(tb testing.TB, ctx context.Context, stdoutPath, want string, timeout time.Duration) string {
	tb.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	drainFifoInto(tb, ctx, stdoutPath, &buf, &mu)
	deadline := time.After(timeout)
	for {
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if strings.Contains(got, want) {
			return got
		}
		select {
		case <-deadline:
			mu.Lock()
			final := buf.String()
			mu.Unlock()
			tb.Fatalf("timed out waiting for %q in stdout, got: %q", want, final)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// sandboxShimPID resolves the shim OS PID via the Task.Connect RPC
// after the first member container exists.  Returns 0 if unavailable.
func sandboxShimPID(env *sandboxEnv, memberCID string) int {
	pid, err := shimPidViaConnect(env.address, memberCID, 2*time.Second)
	if err != nil {
		return 0
	}
	return pid
}

// sandboxMountTargets returns all mount targets visible in the shim
// process's mount namespace by parsing /proc/<pid>/mountinfo.
// Returns nil if pid == 0 or the file is unreadable.
func sandboxMountTargets(pid int) []string {
	if pid == 0 {
		return nil
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/mountinfo", pid))
	if err != nil {
		return nil
	}
	defer f.Close()

	var targets []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// mountinfo: id parent major:minor root mountpoint options ...
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 5 {
			targets = append(targets, fields[4])
		}
	}
	return targets
}

// sandboxContainersMounts returns mount targets that fall under the
// sandbox shared containers directory (i.e. paths containing
// "/containers/").  Used by the mount-leak detector.
func sandboxContainersMounts(pid int) []string {
	all := sandboxMountTargets(pid)
	var matched []string
	for _, t := range all {
		if strings.Contains(t, "/containers/") {
			matched = append(matched, t)
		}
	}
	return matched
}

// waitForSandboxStatus polls SandboxStatus until the state matches
// want or the deadline is exceeded.
func waitForSandboxStatus(ctx context.Context, sc sandboxAPI.TTRPCSandboxService, sandboxID, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := sc.SandboxStatus(ctx, &sandboxAPI.SandboxStatusRequest{SandboxID: sandboxID})
		if err == nil && resp.GetState() == want {
			return nil
		}
		if time.Now().After(deadline) {
			state := "unknown"
			if err == nil {
				state = resp.GetState()
			}
			return fmt.Errorf("timed out waiting for state %q, last state %q (err: %v)", want, state, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Unused import guard: types is used only to satisfy the compiler when
// buildRootfsMountsForSandbox is called from sandbox_suite.go.  The
// function body references the types package via buildEmbeddedRootfs.
var _ *types.Mount

// startSandboxShimWithNetworkSandbox starts a sandbox shim and passes
// networkSandboxPath in the CreateSandboxRequest.  It is otherwise identical
// to startSandboxShim.  Pass an empty string for the host-network case
// (no network sandbox).
//
// The API contract: the shim must hold the host-side network sandbox open
// for the lifetime of the sandbox so that CNI and other host tooling can
// operate on it while the sandbox is running.
func startSandboxShimWithNetworkSandbox(tb testing.TB, cfg Config, sandboxID, networkSandboxPath string) *sandboxEnv {
	tb.Helper()
	return startSandboxShimInner(tb, cfg, sandboxID, networkSandboxPath)
}

// startSandboxShimInner is the shared implementation; exposed via
// startSandboxShim (empty path) and startSandboxShimWithNetworkSandbox.
func startSandboxShimInner(tb testing.TB, cfg Config, sandboxID, networkSandboxPath string) *sandboxEnv {
	tb.Helper()

	shimBin, err := exec.LookPath(cfg.ShimBinary)
	if err != nil {
		tb.Fatalf("shim binary %q not found in PATH: %v", cfg.ShimBinary, err)
	}
	shimDir := filepath.Dir(shimBin)
	if !strings.Contains(os.Getenv("PATH"), shimDir) {
		os.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	bundleDir := tb.TempDir()
	bundleDir, err = filepath.EvalSymlinks(bundleDir)
	if err != nil {
		tb.Fatal("evalSymlinks bundleDir:", err)
	}

	writeSandboxOCISpec(tb, bundleDir)
	startEventsRecorder(tb, bundleDir)

	ns := uniqueTestNamespace(tb, "sandbox")
	ctx := namespaces.WithNamespace(tb.Context(), ns)

	params := startShim(tb, shimBin, bundleDir, sandboxID, ns, cfg)

	if params.Version < 3 {
		tb.Fatalf("sandbox API requires bootstrap version ≥ 3, shim returned %d", params.Version)
	}

	conn := connectShim(tb, params.Address)
	client := ttrpc.NewClient(conn)
	tb.Cleanup(func() { client.Close() })

	sc := sandboxAPI.NewTTRPCSandboxClient(client)
	tc := taskAPI.NewTTRPCTaskClient(client)

	env := &sandboxEnv{
		ctx:        ctx,
		client:     client,
		sc:         sc,
		tc:         tc,
		sandboxID:  sandboxID,
		address:    params.Address,
		stdoutBufs: make(map[string]*containerOutput),
	}

	if _, err := sc.CreateSandbox(ctx, &sandboxAPI.CreateSandboxRequest{
		SandboxID:  sandboxID,
		BundlePath: bundleDir,
		NetnsPath:  networkSandboxPath,
	}); err != nil {
		tb.Fatalf("CreateSandbox: %v", err)
	}

	startResp, err := sc.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{
		SandboxID: sandboxID,
	})
	if err != nil {
		tb.Fatalf("StartSandbox: %v", err)
	}
	if startResp.GetPid() == 0 {
		tb.Error("StartSandbox returned pid=0; shim must report a non-zero pid")
	}
	if ts := startResp.GetCreatedAt(); ts == nil || ts.AsTime().IsZero() {
		tb.Error("StartSandbox returned zero createdAt")
	}

	tb.Cleanup(func() {
		shutdownSandboxShim(tb, env)
	})

	return env
}

// sandboxStatusInfo calls SandboxStatus with verbose=true and returns the
// state string and the Info map.  If the RPC fails the test is failed.
func sandboxStatusInfo(tb testing.TB, env *sandboxEnv) (state string, info map[string]string) {
	tb.Helper()
	resp, err := env.sc.SandboxStatus(env.ctx, &sandboxAPI.SandboxStatusRequest{
		SandboxID: env.sandboxID,
		Verbose:   true,
	})
	if err != nil {
		tb.Fatalf("SandboxStatus: %v", err)
	}
	return resp.GetState(), resp.GetInfo()
}

// execInSandboxContainer execs a process in a running member container and
// returns its stdout output and exit status.  It blocks until the exec
// completes or timeout elapses.  stderr is captured and included in the
// returned output (interleaved) so that callers can inspect error messages.
//
// The API contract: Task.Exec followed by Task.Start(ExecID) must run the
// command inside the container; Task.Wait must return the exit status after
// the process terminates.
func execInSandboxContainer(tb testing.TB, env *sandboxEnv, cid string, args []string, timeout time.Duration) (output string, exitStatus uint32) {
	tb.Helper()

	execID := containerID(tb) // unique exec ID derived from test name

	execStdout, execStderr := createIOFifos(tb, tb.TempDir())
	var outBuf bytes.Buffer
	var outMu sync.Mutex
	drainFifoInto(tb, env.ctx, execStdout, &outBuf, &outMu)
	// Also capture stderr so callers can see error messages from the exec'd process.
	drainFifoInto(tb, env.ctx, execStderr, &outBuf, &outMu)

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: args,
		Cwd:  "/",
		Env:  []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	})
	if err != nil {
		tb.Fatalf("marshal exec process spec: %v", err)
	}
	specAny := &anypb.Any{TypeUrl: procSpec.TypeUrl, Value: procSpec.Value}

	if _, err := env.tc.Exec(env.ctx, &taskAPI.ExecProcessRequest{
		ID:     cid,
		ExecID: execID,
		Stdout: execStdout,
		Stderr: execStderr,
		Spec:   specAny,
	}); err != nil {
		tb.Fatalf("Task.Exec in %s: %v", cid, err)
	}
	if _, err := env.tc.Start(env.ctx, &taskAPI.StartRequest{
		ID:     cid,
		ExecID: execID,
	}); err != nil {
		tb.Fatalf("Task.Start exec %s/%s: %v", cid, execID, err)
	}

	ctx, cancel := context.WithTimeout(env.ctx, timeout)
	defer cancel()

	waitResp, err := env.tc.Wait(ctx, &taskAPI.WaitRequest{
		ID:     cid,
		ExecID: execID,
	})
	if err != nil {
		tb.Fatalf("Task.Wait exec %s/%s: %v", cid, execID, err)
	}

	// Allow a moment for the FIFO data to drain.
	time.Sleep(50 * time.Millisecond)
	outMu.Lock()
	output = outBuf.String()
	outMu.Unlock()

	return output, waitResp.GetExitStatus()
}
