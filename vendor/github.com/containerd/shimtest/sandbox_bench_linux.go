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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
)

// Bench runs every benchmark in the SandboxSuite as a sub-benchmark of b.
func (s *SandboxSuite) Bench(b *testing.B) {
	b.Helper()
	b.Run("ContainerCreate", s.benchContainerCreate)
}

// benchContainerCreate measures the per-container create/start/wait/delete
// cycle inside a single running sandbox VM.  The sandbox is started once
// before the iteration loop so the VM boot cost is paid only once; each
// b.N iteration adds one member container, runs it to completion, and
// removes it.
//
// This benchmark is the sandbox-API counterpart to RunSuite.benchLifecycle.
// Because the VM is shared across all iterations, per-iteration cost reflects
// only the marginal work needed to create and run a new container: rootfs
// assembly on the host, guest bundle/mount/task RPCs, and cleanup.  The
// sandbox start time is reported separately as ms/sandbox-start so the
// amortised overhead is visible.
//
// Reported metrics (all in milliseconds, averaged over b.N):
//
//   - ms/create  — Task.Create RPC (rootfs assembly + guest bundle/mount/task)
//   - ms/start   — Task.Start RPC
//   - ms/wait    — Task.Wait until exit
//   - ms/delete  — Task.Delete RPC (rootfs unshare + cleanup)
//   - ms/total   — sum of the four phases above
//
// Reported once (not per-iteration):
//
//   - ms/sandbox-start — time from sandbox shim launch to StartSandbox response
func (s *SandboxSuite) benchContainerCreate(b *testing.B) {
	shimBin, err := exec.LookPath(s.cfg.ShimBinary)
	if err != nil {
		b.Fatalf("shim binary %q not found in PATH: %v", s.cfg.ShimBinary, err)
	}
	if shimDir := filepath.Dir(shimBin); !strings.Contains(os.Getenv("PATH"), shimDir) {
		os.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	// Pre-build the read-only rootfs images once so per-iteration setup
	// only needs to construct the writable layer.  For the sandbox path,
	// buildSandboxMemberMounts uses these to produce the mounts passed to
	// ShareRootfs on each iteration.
	imgs := buildShimImages(b, s.cfg)

	sandboxID := containerID(b)
	base := containerID(b)

	// ── Start the sandbox (timed separately, not part of b.N loop) ───────
	tSandboxStart := time.Now()
	env := startSandboxShim(b, s.cfg, sandboxID)
	sandboxStartMs := float64(time.Since(tSandboxStart).Microseconds()) / 1000.0

	b.ReportMetric(sandboxStartMs, "ms/sandbox-start")

	// ── Per-iteration state ───────────────────────────────────────────────
	var sumCreate, sumStart, sumWait, sumDelete time.Duration

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()

		cid := fmt.Sprintf("%s-%d", base, i)

		// Build a fresh member-container bundle and rootfs mounts.
		bundleDir := b.TempDir()
		bundleDir, err = filepath.EvalSymlinks(bundleDir)
		if err != nil {
			b.Fatal("resolve member bundle dir:", err)
		}
		rootfsDir := filepath.Join(bundleDir, "rootfs")
		if err := os.MkdirAll(rootfsDir, 0755); err != nil {
			b.Fatal("mkdir rootfs:", err)
		}
		rootfsMounts := buildSandboxMemberMounts(b, s.cfg, imgs, rootfsDir, bundleDir)
		cfg := Config{FormatMounts: s.cfg.FormatMounts}
		createOCISpec(b, bundleDir, []string{"/bin/exit", "0"}, cfg)

		stdoutPath, stderrPath := createIOFifos(b, bundleDir)
		drainFifo(b, env.ctx, stdoutPath)
		drainFifo(b, env.ctx, stderrPath)

		req := newCreateTaskRequest(b, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)

		b.StartTimer()

		// Create
		t := time.Now()
		if _, err := env.tc.Create(env.ctx, req); err != nil {
			b.Fatalf("Create %s: %v", cid, err)
		}
		sumCreate += time.Since(t)

		// Start
		t = time.Now()
		if _, err := env.tc.Start(env.ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
			b.Fatalf("Start %s: %v", cid, err)
		}
		sumStart += time.Since(t)

		// Wait
		t = time.Now()
		if _, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: cid}); err != nil {
			b.Fatalf("Wait %s: %v", cid, err)
		}
		sumWait += time.Since(t)

		// Delete (triggers host-side rootfs cleanup via SharedFS.Unshare)
		t = time.Now()
		if _, err := env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: cid}); err != nil {
			b.Fatalf("Delete %s: %v", cid, err)
		}
		sumDelete += time.Since(t)

		b.StopTimer()

		// Remove from env tracking so memory does not accumulate.
		env.mu.Lock()
		for i, id := range env.containers {
			if id == cid {
				env.containers = append(env.containers[:i], env.containers[i+1:]...)
				break
			}
		}
		delete(env.stdoutBufs, cid)
		env.mu.Unlock()
	}

	n := float64(b.N)
	reportMs := func(d time.Duration, name string) {
		b.ReportMetric(float64(d.Microseconds())/n/1000.0, name)
	}
	reportMs(sumCreate, "ms/create")
	reportMs(sumStart, "ms/start")
	reportMs(sumWait, "ms/wait")
	reportMs(sumDelete, "ms/delete")
	reportMs(sumCreate+sumStart+sumWait+sumDelete, "ms/total")
}

// buildSandboxMemberMounts builds the rootfs mount specs for a sandbox member
// container benchmark iteration.  It mirrors the logic in
// createContainerInSandbox but is optimised for benchmarks: when FormatMounts
// is true the pre-built erofs images are reused; otherwise a bind mount of the
// pre-extracted rootfs dir is returned (same fallback that ShareRootfs handles
// by copying into the shared dir).
func buildSandboxMemberMounts(tb testing.TB, cfg Config, imgs shimImages, rootfsDir, bundleDir string) []*types.Mount {
	tb.Helper()
	if cfg.FormatMounts && os.Getuid() == 0 {
		return buildRootfsMountsFromImages(tb, cfg, imgs, rootfsDir)
	}
	// Non-root or non-format: extract once into rootfsDir, then wrap as
	// a bind mount so ShareRootfs can copy it into the shared directory.
	if os.Getuid() != 0 {
		extractErofsIntoDir(tb, imgs.erofsImg, rootfsDir)
		return []*types.Mount{{
			Type:    "bind",
			Source:  rootfsDir,
			Options: []string{"ro", "rbind"},
		}}
	}
	_ = bundleDir
	return buildRootfsMountsFromImages(tb, cfg, imgs, rootfsDir)
}

// Ensure the context package is used (env.ctx references it implicitly).
var _ context.Context
