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
)

// Bench runs every benchmark in the RunSuite as a sub-benchmark of
// b. Each benchmark times one phase of a container's lifecycle so
// regressions can be localized.
func (s *RunSuite) Bench(b *testing.B) {
	b.Helper()
	b.Run("Lifecycle", s.benchLifecycle)
	b.Run("Startup", s.benchStartup)
	b.Run("StartupPhases", s.benchStartupPhases)
	b.Run("Start", s.benchStart)
}

// benchLifecycle measures the full create-start-kill-wait-delete
// lifecycle of a container through the shim, including the shim
// start. Per-phase durations are reported as custom metrics so
// regressions can be localized to a specific RPC.
func (s *RunSuite) benchLifecycle(b *testing.B) {
	// Build the read-only erofs images once; per-iteration setup only
	// creates the writable ext4 scratch image (or overlay upper/work).
	shimBin, err := exec.LookPath(s.cfg.ShimBinary)
	if err != nil {
		b.Fatalf("shim binary %q not found in PATH: %v", s.cfg.ShimBinary, err)
	}
	if shimDir := filepath.Dir(shimBin); !strings.Contains(os.Getenv("PATH"), shimDir) {
		os.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	}
	imgs := buildShimImages(b, s.cfg)

	base := containerID(b)
	ns := shimtestNamespace

	var sumShim, sumCreate, sumStart, sumKill, sumWait, sumDelete time.Duration

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
		rootfsMounts := buildRootfsMountsFromImages(b, s.cfg, imgs, rootfsDir)

		createOCISpec(b, bundleDir, []string{"/bin/forever", "hello"}, s.cfg)
		stdoutPath, stderrPath := createIOFifos(b, bundleDir)
		ctx := namespaces.WithNamespace(b.Context(), ns)

		var stdoutBuf bytes.Buffer
		var stdoutMu sync.Mutex
		drainFifoInto(b, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
		drainFifo(b, ctx, stderrPath)

		b.StartTimer()

		t := time.Now()
		params := startShim(b, shimBin, bundleDir, cid, ns, s.cfg)
		conn := connectShim(b, params.Address)
		client := ttrpc.NewClient(conn)
		tc := taskAPI.NewTTRPCTaskClient(client)
		sumShim += time.Since(t)

		t = time.Now()
		if _, err := tc.Create(ctx, newCreateTaskRequest(b, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
			b.Fatal("create failed:", err)
		}
		sumCreate += time.Since(t)

		t = time.Now()
		if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
			b.Fatal("start failed:", err)
		}
		sumStart += time.Since(t)

		t = time.Now()
		if _, err := tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true}); err != nil {
			b.Fatal("kill failed:", err)
		}
		sumKill += time.Since(t)

		t = time.Now()
		if _, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid}); err != nil {
			b.Fatal("wait failed:", err)
		}
		sumWait += time.Since(t)

		t = time.Now()
		if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid}); err != nil {
			b.Fatal("delete failed:", err)
		}
		sumDelete += time.Since(t)

		b.StopTimer()
		tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
		client.Close()
	}

	n := float64(b.N)
	reportMs := func(d time.Duration, name string) {
		b.ReportMetric(float64(d.Microseconds())/n/1000.0, name)
	}
	reportMs(sumShim, "ms/shim-start")
	reportMs(sumCreate, "ms/create")
	reportMs(sumStart, "ms/start")
	reportMs(sumKill, "ms/kill")
	reportMs(sumWait, "ms/wait")
	reportMs(sumDelete, "ms/delete")
}

// benchStartup measures the time from shim start through the first
// output being produced by the container process.
func (s *RunSuite) benchStartup(b *testing.B) {
	shimBin, err := exec.LookPath(s.cfg.ShimBinary)
	if err != nil {
		b.Fatalf("shim binary %q not found in PATH: %v", s.cfg.ShimBinary, err)
	}
	if shimDir := filepath.Dir(shimBin); !strings.Contains(os.Getenv("PATH"), shimDir) {
		os.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	}
	imgs := buildShimImages(b, s.cfg)

	base := containerID(b)
	ns := shimtestNamespace

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
		rootfsMounts := buildRootfsMountsFromImages(b, s.cfg, imgs, rootfsDir)

		createOCISpec(b, bundleDir, []string{"/bin/forever", "started"}, s.cfg)
		stdoutPath, stderrPath := createIOFifos(b, bundleDir)
		ctx := namespaces.WithNamespace(b.Context(), ns)

		var stdoutBuf bytes.Buffer
		var stdoutMu sync.Mutex
		drainFifoInto(b, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
		drainFifo(b, ctx, stderrPath)

		b.StartTimer()

		params := startShim(b, shimBin, bundleDir, cid, ns, s.cfg)
		conn := connectShim(b, params.Address)
		client := ttrpc.NewClient(conn)
		tc := taskAPI.NewTTRPCTaskClient(client)

		if _, err := tc.Create(ctx, newCreateTaskRequest(b, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
			b.Fatal("create failed:", err)
		}
		if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
			b.Fatal("start failed:", err)
		}

		deadline := time.After(30 * time.Second)
		for {
			stdoutMu.Lock()
			got := stdoutBuf.String()
			stdoutMu.Unlock()
			if strings.Contains(got, "started") {
				break
			}
			select {
			case <-deadline:
				b.Fatal("timed out waiting for output")
			case <-time.After(1 * time.Millisecond):
			}
		}

		b.StopTimer()
		tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
		tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
		tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
		tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
		client.Close()
	}
}

// benchStartupPhases reports per-phase timings for the startup
// sequence. Custom metrics are averaged over b.N iterations.
func (s *RunSuite) benchStartupPhases(b *testing.B) {
	shimBin, err := exec.LookPath(s.cfg.ShimBinary)
	if err != nil {
		b.Fatalf("shim binary %q not found in PATH: %v", s.cfg.ShimBinary, err)
	}
	if shimDir := filepath.Dir(shimBin); !strings.Contains(os.Getenv("PATH"), shimDir) {
		os.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	}
	imgs := buildShimImages(b, s.cfg)

	base := containerID(b)
	ns := shimtestNamespace

	var sumShimStart, sumConnect, sumCreate, sumTaskStart, sumOutput, sumTotal time.Duration

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
		rootfsMounts := buildRootfsMountsFromImages(b, s.cfg, imgs, rootfsDir)

		createOCISpec(b, bundleDir, []string{"/bin/forever", "started"}, s.cfg)
		stdoutPath, stderrPath := createIOFifos(b, bundleDir)
		ctx := namespaces.WithNamespace(b.Context(), ns)

		var stdoutBuf bytes.Buffer
		var stdoutMu sync.Mutex
		drainFifoInto(b, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
		drainFifo(b, ctx, stderrPath)

		b.StartTimer()
		t0 := time.Now()

		params := startShim(b, shimBin, bundleDir, cid, ns, s.cfg)
		tShimStart := time.Since(t0)

		t1 := time.Now()
		conn := connectShim(b, params.Address)
		client := ttrpc.NewClient(conn)
		tc := taskAPI.NewTTRPCTaskClient(client)
		tConnect := time.Since(t1)

		t2 := time.Now()
		if _, err := tc.Create(ctx, newCreateTaskRequest(b, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
			b.Fatal("create failed:", err)
		}
		tCreate := time.Since(t2)

		t3 := time.Now()
		if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
			b.Fatal("start failed:", err)
		}
		tTaskStart := time.Since(t3)

		t4 := time.Now()
		deadline := time.After(30 * time.Second)
		for {
			stdoutMu.Lock()
			got := stdoutBuf.String()
			stdoutMu.Unlock()
			if strings.Contains(got, "started") {
				break
			}
			select {
			case <-deadline:
				b.Fatal("timed out waiting for output")
			case <-time.After(1 * time.Millisecond):
			}
		}
		tOutput := time.Since(t4)
		tTotal := time.Since(t0)

		b.StopTimer()
		sumShimStart += tShimStart
		sumConnect += tConnect
		sumCreate += tCreate
		sumTaskStart += tTaskStart
		sumOutput += tOutput
		sumTotal += tTotal

		tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
		tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
		tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
		tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
		client.Close()
	}

	n := float64(b.N)
	b.ReportMetric(float64(sumShimStart.Milliseconds())/n, "ms/shim-start")
	b.ReportMetric(float64(sumConnect.Milliseconds())/n, "ms/connect")
	b.ReportMetric(float64(sumCreate.Milliseconds())/n, "ms/create")
	b.ReportMetric(float64(sumTaskStart.Milliseconds())/n, "ms/task-start")
	b.ReportMetric(float64(sumOutput.Milliseconds())/n, "ms/output")
	b.ReportMetric(float64(sumTotal.Milliseconds())/n, "ms/total")
}

// benchStart measures just the shim "start" subcommand: the time
// from fork/exec of the shim binary until it returns its bootstrap
// parameters.
//
// The shim daemon is started and immediately shut down. No container
// task is created within it — the TTRPC Create method is never called
// — so the shim never receives or mounts rootfsMounts. Because the
// rootfs images are unused, they are not built at all: each iteration
// needs only a minimal bundle directory (config.json + log FIFO),
// which avoids O(b.N) erofs accumulation on the tmpfs at the high
// iteration counts this fast benchmark produces.
func (s *RunSuite) benchStart(b *testing.B) {
	// Resolve the shim binary once; a fresh minimal bundle dir is
	// created per iteration (tiny: only config.json + log fifo).
	shimBin, err := exec.LookPath(s.cfg.ShimBinary)
	if err != nil {
		b.Fatalf("shim binary %q not found in PATH: %v", s.cfg.ShimBinary, err)
	}
	if shimDir := filepath.Dir(shimBin); !strings.Contains(os.Getenv("PATH"), shimDir) {
		os.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	}

	base := containerID(b)
	ns := shimtestNamespace

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		cid := fmt.Sprintf("%s-%d", base, i)

		bundleDir := b.TempDir()
		bundleDir, err = filepath.EvalSymlinks(bundleDir)
		if err != nil {
			b.Fatal("resolve bundle dir:", err)
		}
		if err := os.MkdirAll(filepath.Join(bundleDir, "rootfs"), 0755); err != nil {
			b.Fatal("mkdir rootfs:", err)
		}
		createOCISpec(b, bundleDir, []string{"/bin/forever"}, s.cfg)
		ctx := namespaces.WithNamespace(b.Context(), ns)

		b.StartTimer()
		params := startShim(b, shimBin, bundleDir, cid, ns, s.cfg)
		b.StopTimer()

		conn := connectShim(b, params.Address)
		client := ttrpc.NewClient(conn)
		tc := taskAPI.NewTTRPCTaskClient(client)
		tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
		client.Close()
	}
}
