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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
)

// stressSandboxConcurrency is the number of member containers created
// per iteration of the sandbox stress test.
const stressSandboxConcurrency = 3

// stressSandboxMaxRSSGrowth is the upper bound on shim RSS growth (in
// bytes) for the sandbox stress run.  VM-based shims exhibit a large
// one-time RSS step on first boot (guest RAM, VMM structures) that
// saturates quickly; growth beyond that is the signal of a per-container
// leak.
//
// 512 MiB accommodates the observed one-time VM boot step (typically
// ~200–300 MiB on Linux) with headroom.  Per-container growth at
// steady state should be < 1 KiB/container.
const stressSandboxMaxRSSGrowth = 512 * 1024 * 1024

// testSandbox exercises the sandbox shim API under sustained container
// churn: one long-lived sandbox VM hosts repeated bursts of concurrent
// member containers.  Each burst creates, starts, waits, and deletes
// stressSandboxConcurrency containers before the next burst begins.
//
// Leak-detection components:
//
//  1. Process leak: no shim processes remain after the run (enforced
//     by the top-level registerShimLeakCheck in StressSuite.Run).
//  2. Host RSS growth: the shim RSS must not exceed
//     stressSandboxMaxRSSGrowth bytes over the run duration.
//  3. Mount leak: no per-container mount points remain in the shim's
//     mount namespace after ShutdownSandbox.
func (s *StressSuite) testSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sandbox stress in short mode")
	}

	// Pre-build read-only rootfs images once.  Reusing them across all
	// iterations keeps disk consumption O(1) instead of O(iterations).
	// Per-iteration setup only creates the small writable parts (ext4
	// scratch image or overlay upper/work dirs).
	imgs := buildShimImages(t, s.cfg)

	// On non-root systems (where loop mounts are unavailable), pre-extract
	// the rootfs erofs into a single directory that every iteration reuses
	// as the bind-mount source.  ShareRootfs copies it into the sandbox
	// shared dir per container and Unshare removes it promptly, so disk
	// consumption stays O(1) across the run.
	var preExtractedRootfs string
	if !s.cfg.FormatMounts || os.Getuid() != 0 {
		preExtractedRootfs = t.TempDir()
		extractErofsIntoDir(t, imgs.erofsImg, preExtractedRootfs)
	}

	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	// Bootstrap: create a probe container to seed the shimPID lookup
	// and establish that the sandbox is functional before the loop.
	probeCID := createContainerInSandbox(t, env, []string{"/bin/echo", "probe"})
	readContainerOutput(t, env, probeCID, "probe", 30*time.Second)
	env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: probeCID}) //nolint:errcheck

	shimPID := sandboxShimPID(env, probeCID)
	releaseSandboxContainer(env.ctx, env, probeCID) //nolint:errcheck
	t.Logf("sandbox stress: shim PID=%d", shimPID)

	// Sample RSS before the churn loop.
	var rssBefore int64
	var rssOK bool
	if shimPID != 0 {
		var err error
		rssBefore, err = readRSS(shimPID)
		if err != nil {
			t.Logf("cannot read pre-stress shim RSS (PID %d): %v — disabling RSS check", shimPID, err)
		} else {
			rssOK = true
		}
	}

	var iterIdx atomic.Int64
	ctx, cancel := stressCtx(t, env.ctx)
	defer cancel()

	iters, elapsed, stressErr := runStress(ctx, func(iterCtx context.Context) error {
		i := iterIdx.Add(1)
		name := fmt.Sprintf("sbiter%05d", i)

		// Each iteration creates stressSandboxConcurrency containers using
		// the pre-built images.  Commands cycle:
		//   j%3 == 0: /bin/exit 0 (exit-code propagation)
		//   j%3 == 1: /bin/exit 0
		//   j%3 == 2: /bin/exit 0
		// All containers exit cleanly; we just verify the exit status.
		// Output is discarded (stdout/stderr FIFOs are drained silently)
		// so the test exercises the create/start/wait/delete path under
		// sustained load without accumulating output buffers.
		type ctrInfo struct {
			cid string
		}
		ctrs := make([]ctrInfo, stressSandboxConcurrency)
		for j := range ctrs {
			args := []string{"/bin/echo", fmt.Sprintf("%s-j%d", name, j)}
			cid, err := createSandboxContainerFast(iterCtx, t, env, s.cfg, imgs, preExtractedRootfs, args)
			if err != nil {
				return fmt.Errorf("create %d: %w", j, err)
			}
			ctrs[j] = ctrInfo{cid: cid}
		}

		// Wait for all containers concurrently.
		var wg sync.WaitGroup
		errs := make([]error, stressSandboxConcurrency)
		for j, ci := range ctrs {
			wg.Add(1)
			go func(j int, ci ctrInfo) {
				defer wg.Done()
				subCtx, subCancel := context.WithTimeout(iterCtx, stressIterationTimeout)
				defer subCancel()

				waitResp, err := env.tc.Wait(subCtx, &taskAPI.WaitRequest{ID: ci.cid})
				if err != nil {
					errs[j] = fmt.Errorf("wait %s: %w", ci.cid, err)
					return
				}
				if waitResp.GetExitStatus() != 0 {
					errs[j] = fmt.Errorf("container %s exited with status %d",
						ci.cid, waitResp.GetExitStatus())
				}
			}(j, ci)
		}
		wg.Wait()

		for _, e := range errs {
			if e != nil {
				return e
			}
		}

		// Delete all containers and release tracking state immediately.
		for _, ci := range ctrs {
			if err := releaseSandboxContainer(iterCtx, env, ci.cid); err != nil {
				return fmt.Errorf("delete %s: %w", ci.cid, err)
			}
		}

		return nil
	})

	rate := float64(iters) / elapsed.Seconds()
	t.Logf("sandbox stress: %d iterations × %d containers = %d total containers in %s (%.1f iter/s)",
		iters, stressSandboxConcurrency, iters*stressSandboxConcurrency,
		elapsed.Round(time.Millisecond), rate)

	if stressErr != nil {
		t.Fatalf("sandbox stress: %v", stressErr)
	}

	// ── RSS growth check ──────────────────────────────────────────────
	if rssOK && shimPID != 0 {
		rssAfter, err := readRSS(shimPID)
		if err != nil {
			t.Logf("cannot read post-stress shim RSS: %v", err)
		} else {
			growth := rssAfter - rssBefore
			threshold := int64(stressSandboxMaxRSSGrowth)
			if s.options.SandboxRSSGrowthOverride > 0 {
				threshold = s.options.SandboxRSSGrowthOverride
			}
			t.Logf("shim RSS: before=%d MiB  after=%d MiB  growth=%d MiB  (threshold %d MiB)",
				rssBefore>>20, rssAfter>>20, growth>>20, threshold>>20)
			if growth > threshold {
				t.Errorf("shim RSS grew %d bytes during sandbox stress (threshold %d bytes); "+
					"possible per-container memory leak",
					growth, threshold)
			}
		}
	}

	// ── Mount-leak check ─────────────────────────────────────────────
	// Snapshot mounts before shutdown, then verify they are gone after.
	mountsBefore := sandboxContainersMounts(shimPID)
	t.Logf("per-container mounts before shutdown: %d", len(mountsBefore))

	// Trigger shutdown (cleanup is also registered by startSandboxShim,
	// but we drive it explicitly here so we can inspect state after).
	env.sc.StopSandbox(env.ctx, &sandboxAPI.StopSandboxRequest{SandboxID: sandboxID})         //nolint:errcheck
	env.sc.ShutdownSandbox(env.ctx, &sandboxAPI.ShutdownSandboxRequest{SandboxID: sandboxID}) //nolint:errcheck

	// Allow shim to finish cleanup.
	time.Sleep(500 * time.Millisecond)

	mountsAfter := sandboxContainersMounts(shimPID)
	if len(mountsAfter) > 0 {
		t.Errorf("sandbox stress: %d per-container mount(s) leaked after ShutdownSandbox: %v",
			len(mountsAfter), mountsAfter)
	} else {
		t.Log("no per-container mounts remain after shutdown (mount-leak check passed)")
	}
}
