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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
)

// Bench runs every benchmark in the LayersSuite as a sub-benchmark
// of b.
func (s *LayersSuite) Bench(b *testing.B) {
	b.Helper()
	b.Run("ThirtyLayers", s.benchThirtyLayers)
}

// benchThirtyLayers measures the cost of bringing up a container
// whose rootfs is built from 30 stacked erofs layers (the same
// layout as HundredLayers, just smaller — each layer adds one file
// and whites out one base file). Each iteration creates a fresh
// shim+container and tears it down; the timed region covers shim
// start through task start, isolating the overhead of mounting
// many lowers. Per-phase durations are reported as custom metrics
// so regressions can be localized to a specific RPC.
//
// Only meaningful when cfg.FormatMounts is true (the shim assembles
// the multi-layer overlay itself); skipped otherwise.
//
// The 30-layer erofs images and the rootfs erofs are built once
// outside the loop and reused across iterations — they're
// content-identical, immutable, and re-mountable, so per-iteration
// rebuilding would just add noise. Each iteration does get its own
// ext4 scratch (the writable upper) via buildErofsMounts.
func (s *LayersSuite) benchThirtyLayers(b *testing.B) {
	if !s.cfg.FormatMounts {
		b.Skip("layers benchmark requires FormatMounts mode")
	}

	const numLayers = 30
	base := containerID(b)
	ns := shimtestNamespace

	shimBin, err := exec.LookPath(s.cfg.ShimBinary)
	if err != nil {
		b.Fatalf("shim binary %q not found in PATH: %v", s.cfg.ShimBinary, err)
	}
	shimDir := filepath.Dir(shimBin)
	if !strings.Contains(os.Getenv("PATH"), shimDir) {
		os.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	}

	// Build the read-only erofs images once; identical across
	// iterations, so per-iteration rebuilds would just measure
	// erofs.Writer throughput rather than shim multi-layer cost.
	layerImgs := writeLayersErofs(b, numLayers)
	rootfsImg := writeRootfsErofs(b)
	erofsImgs := append(layerImgs, rootfsImg)

	var sumShimStart, sumCreate, sumTaskStart, sumTotal time.Duration

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		cid := fmt.Sprintf("%s-%d", base, i)

		bundleDir := b.TempDir()
		bundleDir, err = filepath.EvalSymlinks(bundleDir)
		if err != nil {
			b.Fatal("resolve bundle dir:", err)
		}
		rootfsDir := filepath.Join(bundleDir, "rootfs")
		if err := os.MkdirAll(rootfsDir, 0755); err != nil {
			b.Fatal("mkdir rootfs:", err)
		}
		b.Cleanup(func() { mount.Unmount(rootfsDir, 0) })

		// Fresh ext4 scratch per iteration (writable upper layer).
		// The erofs lowers are reused.
		rootfsMounts := buildErofsMounts(b, erofsImgs)

		createOCISpec(b, bundleDir, []string{"/bin/forever"}, s.cfg)
		stdoutPath, stderrPath := createIOFifos(b, bundleDir)
		ctx := namespaces.WithNamespace(b.Context(), ns)
		drainFifo(b, ctx, stdoutPath)
		drainFifo(b, ctx, stderrPath)

		b.StartTimer()
		t0 := time.Now()

		params := startShim(b, shimBin, bundleDir, cid, ns, s.cfg)
		conn := connectShim(b, params.Address)
		client := ttrpc.NewClient(conn)
		tc := taskAPI.NewTTRPCTaskClient(client)
		tShimStart := time.Since(t0)

		t1 := time.Now()
		if _, err := tc.Create(ctx, newCreateTaskRequest(b, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
			b.Fatal("create failed:", err)
		}
		tCreate := time.Since(t1)

		t2 := time.Now()
		if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
			b.Fatal("start failed:", err)
		}
		tTaskStart := time.Since(t2)
		tTotal := time.Since(t0)

		b.StopTimer()
		sumShimStart += tShimStart
		sumCreate += tCreate
		sumTaskStart += tTaskStart
		sumTotal += tTotal

		tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
		tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
		tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
		tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
		client.Close()
	}

	n := float64(b.N)
	b.ReportMetric(float64(sumShimStart.Microseconds())/n/1000.0, "ms/shim-start")
	b.ReportMetric(float64(sumCreate.Microseconds())/n/1000.0, "ms/create")
	b.ReportMetric(float64(sumTaskStart.Microseconds())/n/1000.0, "ms/task-start")
	b.ReportMetric(float64(sumTotal.Microseconds())/n/1000.0, "ms/total")
	b.ReportMetric(float64(numLayers), "layers")
}
