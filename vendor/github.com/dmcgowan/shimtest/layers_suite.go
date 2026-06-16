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
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// LayersSuite contains tests gated on the "layers" feature: stacking
// many overlay layers (with file additions and overlayfs whiteout
// removals) into a single rootfs and verifying the merged view.
type LayersSuite struct {
	cfg Config
}

// NewLayersSuite constructs a LayersSuite from the given options.
func NewLayersSuite(cfg Config) *LayersSuite {
	return &LayersSuite{cfg: cfg}
}

// Run runs every test in the suite as a subtest of t.
func (s *LayersSuite) Run(t *testing.T) {
	t.Helper()
	registerShimLeakCheck(t, s.cfg.ShimBinary)
	t.Run("HundredLayers", s.testHundredLayers)
}

// testHundredLayers builds a rootfs from the embedded testbin erofs
// plus 100 additional erofs layers stacked on top:
//
//   - Layer 1 seeds /base with 99 files (base_0..base_98) and adds
//     /added/file_1.
//   - Layers 2..100 each add /added/file_K and white out
//     /base/base_{K-2} via an overlayfs char-dev whiteout.
//
// Inside the running container it execs /bin/ls /added /base to verify
// that all 100 added files are present by name and that /base is empty
// (all whiteout deletions were applied).
//
// Only meaningful when cfg.FormatMounts is true: in that mode the
// shim itself loops the erofs images and assembles the overlay, so
// the test exercises the shim's multi-layer mount-descriptor
// handling end to end. In non-FormatMounts modes the harness
// pre-mounts (or extracts) the rootfs and the shim never sees the
// individual layers, so the test is skipped.
func (s *LayersSuite) testHundredLayers(t *testing.T) {
	if !s.cfg.FormatMounts {
		t.Skip("layers test requires FormatMounts mode (multi-layer rootfs descriptor)")
	}

	const numLayers = 100

	shimBin, err := exec.LookPath(s.cfg.ShimBinary)
	if err != nil {
		t.Fatalf("shim binary %q not found in PATH: %v", s.cfg.ShimBinary, err)
	}
	// Match shimSetup: ensure sibling helpers next to the shim
	// binary resolve via PATH.
	shimDir := filepath.Dir(shimBin)
	if !strings.Contains(os.Getenv("PATH"), shimDir) {
		os.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	}

	bundleDir := t.TempDir()
	bundleDir, err = filepath.EvalSymlinks(bundleDir)
	if err != nil {
		t.Fatal("resolve bundle dir:", err)
	}
	rootfsDir := filepath.Join(bundleDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		t.Fatal("mkdir rootfs:", err)
	}
	t.Cleanup(func() { mount.Unmount(rootfsDir, 0) })

	// Build the rootfs mounts manually: 100 test layers stacked on
	// top of the embedded testbin rootfs erofs. erofsImgs is
	// top-first (index 0 = highest priority in lowerdir).
	layerImgs := writeLayersErofs(t, numLayers)
	rootfsImg := writeRootfsErofs(t)
	erofsImgs := append(layerImgs, rootfsImg)
	rootfsMounts := buildErofsMounts(t, erofsImgs)

	cid := containerID(t)
	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "layers")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	drainFifo(t, ctx, stdoutPath)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		t.Fatal("start failed:", err)
	}

	// runLs execs /bin/ls <dir> inside the container and returns
	// (stdout, stderr, exitStatus).
	runLs := func(dir, execID string) (string, string, uint32) {
		execStdout, execStderr := createIOFifos(t, t.TempDir())
		var stdoutBuf, stderrBuf bytes.Buffer
		var stdoutMu, stderrMu sync.Mutex
		drainFifoInto(t, ctx, execStdout, &stdoutBuf, &stdoutMu)
		drainFifoInto(t, ctx, execStderr, &stderrBuf, &stderrMu)

		procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
			Args: []string{"/bin/ls", dir},
			Cwd:  "/",
			Env:  []string{"PATH=/bin:/usr/bin"},
		})
		if err != nil {
			t.Fatal("marshal exec spec:", err)
		}
		if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
			ID:     cid,
			ExecID: execID,
			Spec:   procSpec,
			Stdout: execStdout,
			Stderr: execStderr,
		}); err != nil {
			t.Fatal("exec failed:", err)
		}
		if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid, ExecID: execID}); err != nil {
			t.Fatal("exec start failed:", err)
		}
		waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid, ExecID: execID})
		if err != nil {
			t.Fatal("exec wait failed:", err)
		}
		// Drain pipes a hair longer than Wait — under load some shims
		// don't have stdout fully flushed at the moment Wait returns.
		time.Sleep(200 * time.Millisecond)
		stdoutMu.Lock()
		out := strings.TrimSpace(stdoutBuf.String())
		stdoutMu.Unlock()
		stderrMu.Lock()
		errOut := strings.TrimSpace(stderrBuf.String())
		stderrMu.Unlock()
		return out, errOut, waitResp.ExitStatus
	}

	// Verify all added files are present.
	addedOut, addedErr, addedExit := runLs("/added", "ls-added")
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid, ExecID: "ls-added"})
	if addedExit != 0 {
		t.Fatalf("ls /added exited %d\nstdout: %s\nstderr: %s", addedExit, addedOut, addedErr)
	}
	addedFiles := make(map[string]bool)
	for _, name := range strings.Split(addedOut, "\n") {
		if name = strings.TrimSpace(name); name != "" {
			addedFiles[name] = true
		}
	}
	var missing []string
	for i := 1; i <= numLayers; i++ {
		if name := fmt.Sprintf("file_%d", i); !addedFiles[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("ls /added missing files: %v\nstdout: %s\nstderr: %s", missing, addedOut, addedErr)
	}
	t.Logf("ls /added: %d files present", len(addedFiles))

	// Verify /base is empty (all whiteouts applied).
	baseOut, baseErr, baseExit := runLs("/base", "ls-base")
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid, ExecID: "ls-base"})
	if baseExit != 0 {
		t.Fatalf("ls /base exited %d\nstdout: %s\nstderr: %s", baseExit, baseOut, baseErr)
	}
	var baseFiles []string
	for _, name := range strings.Split(baseOut, "\n") {
		if name = strings.TrimSpace(name); name != "" {
			baseFiles = append(baseFiles, name)
		}
	}
	if len(baseFiles) != 0 {
		t.Fatalf("ls /base: expected empty, got %v\nstderr: %s", baseFiles, baseErr)
	}
	t.Logf("ls /base: empty (ok)")
	tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	shutdownTask(ctx, tc, cid)
}
