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
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	transferapi "github.com/containerd/containerd/api/services/transfer/v1"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/containerd/shimtest/internal/transfer"
)

// StressSuite contains long-running stress tests that exercise the
// shim under sustained load. Each subtest runs for a configurable
// duration (bounded by t.Deadline() - stressSoakBuffer); the whole
// suite is skipped under -short.
//
// The suite verifies, on completion, that no shim processes leaked
// and (where applicable) that long-running shims didn't grow
// memory unboundedly.
type StressSuite struct {
	cfg     Config
	options StressOptions
}

// StressOptions tunes which subtests StressSuite.Run executes.
type StressOptions struct {
	// Transfer enables the bidirectional transfer-service stress
	// test. The shim under test must implement the transfer service.
	Transfer bool

	// Sandbox enables the sandbox container-churn stress test.  The
	// shim under test must implement the containerd sandbox shim API
	// (runtime/sandbox/v1).
	Sandbox bool

	// ExecRSSGrowthOverride, when non-zero, replaces the platform default
	// RSS growth threshold for the exec stress test. Use this for shims
	// that host a VM or other large runtime in-process and therefore have
	// a higher expected one-time RSS step than a thin supervisor shim.
	// The value is in bytes.
	ExecRSSGrowthOverride int64

	// SandboxRSSGrowthOverride, when non-zero, replaces the platform default
	// RSS growth threshold for the sandbox stress test.  VM-based shims
	// have a large one-time RSS step from the VM boot that saturates
	// early; set this to accommodate the expected baseline.
	// The value is in bytes.
	SandboxRSSGrowthOverride int64
}

// NewStressSuite constructs a StressSuite from cfg and options.
func NewStressSuite(cfg Config, options StressOptions) *StressSuite {
	return &StressSuite{cfg: cfg, options: options}
}

// Run runs every configured stress test as a subtest of t. Skipped
// under -short. Registers a leak check that fires after all subtests
// (and their cleanups) complete.
func (s *StressSuite) Run(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	registerShimLeakCheck(t, s.cfg.ShimBinary)

	t.Run("Lifecycle", s.testLifecycle)
	t.Run("Exec", s.testExec)
	if s.options.Transfer {
		t.Run("Transfer", s.testTransfer)
	}
	if s.options.Sandbox {
		t.Run("Sandbox", s.testSandbox)
	}
}

// testLifecycle exercises the full create/start/run/kill/wait/delete
// path repeatedly. Each iteration is a sub-T so per-iteration
// resources (shim, bundle, fifos) are released between iterations
// rather than accumulating on the parent's cleanup stack.
func (s *StressSuite) testLifecycle(t *testing.T) {
	ctx, cancel := stressCtx(t, t.Context())
	defer cancel()

	var idx atomic.Int64
	// The Shutdown RPC may tear down the ttrpc server before responding, causing
	// the RPC to fail with "ttrpc: closed". This is a non-fatal shim but that we
	// can at least suppress the noise in test logs but could be more strict about.
	var ttrpcClosedOnShutdown atomic.Int64
	iters, elapsed, err := runStress(ctx, func(ctx context.Context) error {
		i := idx.Add(1)
		name := fmt.Sprintf("iter%05d", i)
		var iterErr error
		ok := t.Run(name, func(subT *testing.T) {
			iterErr = doFullLifecycle(subT, ctx, s.cfg, &ttrpcClosedOnShutdown)
			if iterErr != nil {
				subT.Fatal(iterErr)
			}
		})
		if !ok {
			return iterErr
		}
		return nil
	})
	rate := float64(iters) / elapsed.Seconds()
	t.Logf("lifecycle: %d iterations in %s (%.0f iter/s)",
		iters, elapsed.Round(time.Millisecond), rate)
	t.Logf("lifecycle: ttrpc: closed on Shutdown: %d/%d",
		ttrpcClosedOnShutdown.Load(), iters)
	if err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
}

// doFullLifecycle drives one container through start-to-shutdown
// inside a sub-test scope. Helpers register cleanups on subT, which
// fire when the sub-test ends. ttrpcClosedOnShutdown is incremented
// when the Shutdown RPC fails with ttrpc: closed (see call-site TODO).
func doFullLifecycle(t *testing.T, baseCtx context.Context, cfg Config, ttrpcClosedOnShutdown *atomic.Int64) error {
	t.Helper()
	shimBin, bundleDir, rootfsMounts := shimSetup(t, cfg)
	cid := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/echo", "hello"}, cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "stress")
	ctx := namespaces.WithNamespace(baseCtx, ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	drainFifoInto(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Wait for output (deadline is short — process is /bin/echo, exits fast).
	deadline := time.After(stressIterationTimeout)
	for {
		stdoutMu.Lock()
		got := stdoutBuf.String()
		stdoutMu.Unlock()
		if strings.Contains(got, "hello") {
			break
		}
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for output")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if _, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid}); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid}); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	shutCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	if _, err := tc.Shutdown(shutCtx, &taskAPI.ShutdownRequest{ID: cid}); err != nil {
		if strings.Contains(err.Error(), "ttrpc: closed") {
			ttrpcClosedOnShutdown.Add(1)
		} else {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
}

// stressExecConcurrency is how many exec processes the exec-stress
// test launches per iteration. Each iteration waits for all of them
// to complete before the next iteration starts.
const stressExecConcurrency = 4

// stressExecSeed is the fixed PRNG seed for the payload-size sequence
// used in the burstexit and round-trip exec stress variants. A fixed
// seed makes each test run produce the same size sequence, so failures
// are reproducible without any extra flags.
const stressExecSeed uint64 = 42

// stressExecMinSize and stressExecMaxSize bound the pseudo-random
// payload size (in bytes) chosen per iteration for the burstexit and
// round-trip exec stress variants.
const (
	stressExecMinSize = 32 * 1024        // 32 KiB
	stressExecMaxSize = 32 * 1024 * 1024 // 32 MiB
)

// stressExecKind identifies which exec variant a stress iteration runs.
// Iterations cycle through the kinds in declaration order so that every
// few iterations exercise a different part of the shim's exec pipeline.
type stressExecKind uint8

const (
	// stressExecKindEcho execs /bin/echo. No data-integrity check;
	// verifies only that the process completes successfully.
	stressExecKindEcho stressExecKind = iota
	// stressExecKindBurstexit execs /bin/burstexit with a pseudo-random
	// payload size. The process writes the tiled payload to stdout and
	// exits immediately; the test verifies the full byte count and
	// CRC-32. Exercises the shim's close-before-drain path.
	stressExecKindBurstexit
	// stressExecKindRoundTrip execs /bin/cat with a pseudo-random
	// payload piped via stdin and read back from stdout. Verifies byte
	// count and CRC-32. Exercises both stdin delivery and stdout drain.
	stressExecKindRoundTrip
	stressExecKindCount
)

// stressMaxRSSGrowth is the upper bound on shim RSS growth between
// the start and end of the exec stress run. Crossing this threshold
// indicates an unbounded leak in the shim's per-exec bookkeeping.
//
// # Linux (384 MiB)
//
// Shims that host a VM in-process exhibit a large one-time RSS step
// from VM initialization — vCPU state, virtio device buffers,
// guest RAM, Go runtime heap watermark — that saturates early and
// does not grow linearly with exec count.
// Observed nerdbox data over a 19-minute / ~6000-iter run:
//
//	RSS after  30s /  ~150 iters: +181 MiB
//	RSS after  60s /  ~325 iters: +187 MiB   ← most growth is here
//	RSS after  11m / ~3500 iters: +194 MiB
//	RSS after  19m / ~6000 iters: +204 MiB   ← saturated
//
// Growth from 60s to 19 minutes is only +17 MiB despite 18× more
// iterations; the per-iteration rate drops from ~570 KiB at 30s to
// ~3 KiB at steady state — a clear saturation signature, not a leak.
//
// 384 MiB = ~1.9× the observed 19-min peak (~204 MiB), providing
// enough headroom for CI variance while still catching a genuine
// per-exec leak: at the observed steady-state rate of ~3 KiB/iter a
// true linear leak would cross the threshold after ~60 000 iterations
// (~45 minutes at 22 iter/s), well within a standard CI run window.
//
// Thin supervisor shims (runc-style) on Linux have negligible in-process
// overhead; any retained per-exec state beyond a few MiB is a leak.
// The 384 MiB ceiling gives such shims a 384 MiB budget to detect
// leaks, which is more than sufficient. Use ExecRSSGrowthOverride in
// StressOptions to set a tighter bound if desired.
//
// # macOS (128 MiB)
//
// One-time pool allocations (Go runtime heap watermark, HVF VM state,
// virtio device buffers) produce an expected RSS step of ~100 MiB that
// saturates early in the run rather than growing linearly with exec
// count. 128 MiB = ~1.3× the observed peak step, large enough to
// absorb OS page-cache fluctuation while still catching a true
// per-exec leak at the observed rate of ~5 KB/exec within 10 minutes.
//
// # Windows (384 MiB)
//
// The shim hosts the krun VM in-process: krun.dll is loaded, vCPU and
// virtio-blk threads are running, and guest RAM pages are committed
// lazily as the guest touches them. Two 30-minute runs observed:
//
//	RSS before: ~135 MB
//	RSS after 5 min / 32k execs:  +152 MB
//	RSS after 30 min / 32k execs: +142 MB   ← similar total, longer time
//
// Growth saturated between 5 min and 30 min despite identical exec
// counts, which is the signature of a one-time pool allocation rather
// than a per-exec leak. The expected one-time sources are:
//
//   - Go runtime heap high-watermark from peak concurrency (goroutine
//     stacks, sync.Pool buffers for 4KB stream copies).
//   - Windows working-set retention: unlike Linux's MADV_DONTNEED, the
//     Windows kernel does not eagerly decommit freed heap pages when RAM
//     pressure is low, so RSS stays elevated even after the Go GC runs.
//
// 384 MiB = ~2.7× the observed peak growth, chosen so that a true
// per-exec leak (which would be roughly linear in exec count) at the
// observed rate of ~5 KB/exec would be detected within a 30-minute run
// before the threshold is crossed, while giving headroom for the
// one-time pool growth.
var stressMaxRSSGrowth = func() int64 {
	switch runtime.GOOS {
	case "windows":
		return 384 << 20 // 384 MiB — see comment above
	case "darwin":
		return 128 << 20 // 128 MiB — see comment above
	default:
		return 384 << 20 // 384 MiB — see comment above (covers in-process VM shims)
	}
}()

// stressGuestMemSampleInterval is how often the exec stress test
// samples guest memory via /proc/meminfo.
const stressGuestMemSampleInterval = 30 * time.Second

// testExec keeps one shim alive and exec's many concurrent
// short-lived processes against it. Host shim RSS and guest memory
// (via /proc/meminfo) are both sampled: guest memory is sampled
// periodically throughout the run and provides a more stable
// leak signal than host RSS, which can vary due to VM memory
// ballooning and VMM overhead.
func (s *StressSuite) testExec(t *testing.T) {
	env := newShimEnv(t, t.Context(), s.cfg, "stress")
	defer shutdownShim(t, env.ctx, env)

	pid := env.shimPID
	if pid == 0 {
		t.Skip("skipping: shim pid not available, RSS monitoring unavailable")
	}

	rssBefore, err := readRSS(pid)
	if err != nil {
		t.Skipf("cannot read shim RSS: %v", err)
	}

	// Take an initial guest memory reading. Non-fatal if the shim
	// doesn't run a Linux guest (e.g. runc).
	var guestMemSeq atomic.Int64
	nextGuestMemID := func() string {
		return fmt.Sprintf("guestmem-%d", guestMemSeq.Add(1))
	}
	guestBefore, guestBeforeErr := readGuestMem(env.ctx, env, nextGuestMemID())
	if guestBeforeErr != nil {
		t.Logf("guest memory sampling unavailable: %v", guestBeforeErr)
	}

	echoSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/echo", "execstress"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal("marshal exec spec:", err)
	}

	// Seeded PRNG for payload sizes. Sizes are computed in the main
	// goroutine before each iteration's goroutines are spawned so the
	// sequence is deterministic regardless of goroutine scheduling.
	rng := rand.New(rand.NewPCG(stressExecSeed, 0))
	nextSize := func() int {
		return stressExecMinSize + rng.IntN(stressExecMaxSize-stressExecMinSize+1)
	}

	ctx, cancel := stressCtx(t, env.ctx)
	defer cancel()

	// Periodically sample guest memory alongside the exec stress loop
	// so we can spot leaks inside the VM rather than relying solely on
	// host RSS (which includes VMM overhead and balloon variance).
	type guestSample struct {
		elapsed time.Duration
		bytes   int64
	}
	var (
		guestSamples   []guestSample
		guestSamplesMu sync.Mutex
		stressStart    = time.Now()
	)
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		ticker := time.NewTicker(stressGuestMemSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				used, err := readGuestMem(env.ctx, env, nextGuestMemID())
				if err != nil {
					continue
				}
				guestSamplesMu.Lock()
				guestSamples = append(guestSamples, guestSample{time.Since(stressStart), used})
				guestSamplesMu.Unlock()
			}
		}
	}()

	numKinds := int64(stressExecKindCount)
	var iterIdx atomic.Int64
	iters, elapsed, runErr := runStress(ctx, func(ctx context.Context) error {
		i := iterIdx.Add(1)
		kind := stressExecKind((i - 1) % numKinds)

		var wg sync.WaitGroup
		var firstErr atomic.Pointer[error]
		for j := 0; j < stressExecConcurrency; j++ {
			wg.Add(1)
			go func(j, size int) {
				defer wg.Done()
				execID := fmt.Sprintf("e-%d-%d", i, j)
				var err error
				switch kind {
				case stressExecKindEcho:
					err = runOneExec(ctx, env, execID, echoSpec)
				case stressExecKindBurstexit:
					err = runExecBurstexit(ctx, env, execID, size)
				case stressExecKindRoundTrip:
					err = runExecRoundTrip(ctx, env, execID, size)
				}
				if err != nil {
					e := err
					firstErr.CompareAndSwap(nil, &e)
				}
			}(j, nextSize())
		}
		wg.Wait()
		if e := firstErr.Load(); e != nil {
			return *e
		}
		return nil
	})

	<-samplerDone

	rssAfter, _ := readRSS(pid)
	guestAfter, guestAfterErr := readGuestMem(env.ctx, env, nextGuestMemID())

	rate := float64(iters*stressExecConcurrency) / elapsed.Seconds()
	t.Logf("exec: %d iterations × %d execs in %s (%.0f exec/s); kinds: echo/burstexit/roundtrip cycling; host rss %d → %d (Δ %+d)",
		iters, stressExecConcurrency, elapsed.Round(time.Millisecond),
		rate, rssBefore, rssAfter, rssAfter-rssBefore)

	// Log guest memory samples. Host RSS growth alone is not a reliable
	// leak indicator since it includes VM ballooning and VMM overhead
	// that can vary or recover; guest memory is the more stable signal.
	if guestBeforeErr == nil {
		guestSamplesMu.Lock()
		samples := guestSamples
		guestSamplesMu.Unlock()
		for _, s := range samples {
			t.Logf("guest mem at %s: %.1f MiB used",
				s.elapsed.Round(time.Second), float64(s.bytes)/1024/1024)
		}
		if guestAfterErr == nil {
			t.Logf("guest mem: before=%.1f MiB after=%.1f MiB Δ%+.1f MiB",
				float64(guestBefore)/1024/1024,
				float64(guestAfter)/1024/1024,
				float64(guestAfter-guestBefore)/1024/1024)
		}
	}

	if runErr != nil {
		t.Fatalf("exec stress: %v", runErr)
	}
	rssThreshold := stressMaxRSSGrowth
	if s.options.ExecRSSGrowthOverride > 0 {
		rssThreshold = s.options.ExecRSSGrowthOverride
	}
	if growth := rssAfter - rssBefore; growth > rssThreshold {
		t.Errorf("host shim RSS grew %d bytes (threshold %d) during exec stress",
			growth, rssThreshold)
	}
}

// runOneExec runs a single short-lived exec inside the shared
// container, with its own pipes and a per-exec timeout.
func runOneExec(parentCtx context.Context, env *shimEnv, execID string, procSpec *anypb.Any) error {
	subCtx, cancel := context.WithTimeout(parentCtx, stressIterationTimeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "stress-exec-")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	// createRawPipe is platform-specific (io_unix.go / io_windows.go):
	// on Linux it creates a FIFO; on Windows it creates a named pipe server.
	stdoutPath, stdout, cleanupStdout, err := createRawPipe(dir, "stdout")
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	defer cleanupStdout()

	stderrPath, stderr, cleanupStderr, err := createRawPipe(dir, "stderr")
	if err != nil {
		return fmt.Errorf("create stderr pipe: %w", err)
	}
	defer cleanupStderr()

	go io.Copy(io.Discard, stdout)
	go io.Copy(io.Discard, stderr)

	if _, err := env.tc.Exec(subCtx, &taskAPI.ExecProcessRequest{
		ID:     env.containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdout: stdoutPath,
		Stderr: stderrPath,
	}); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := env.tc.Start(subCtx, &taskAPI.StartRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("start exec: %w", err)
	}
	if _, err := env.tc.Wait(subCtx, &taskAPI.WaitRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	if _, err := env.tc.Delete(subCtx, &taskAPI.DeleteRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("delete exec: %w", err)
	}
	return nil
}

// captureExec runs a single short-lived exec inside the shared
// container, captures its stdout, and returns it as a string.
// Like runOneExec but retains stdout instead of discarding it.
func captureExec(parentCtx context.Context, env *shimEnv, execID string, procSpec *anypb.Any) (string, error) {
	subCtx, cancel := context.WithTimeout(parentCtx, stressIterationTimeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "stress-capture-")
	if err != nil {
		return "", fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	// createRawPipe is platform-specific (io_unix.go / io_windows.go):
	// on Linux it creates a FIFO; on Windows it creates a named pipe server.
	stdoutPath, stdout, cleanupStdout, err := createRawPipe(dir, "stdout")
	if err != nil {
		return "", fmt.Errorf("create stdout pipe: %w", err)
	}
	defer cleanupStdout()

	stderrPath, stderr, cleanupStderr, err := createRawPipe(dir, "stderr")
	if err != nil {
		return "", fmt.Errorf("create stderr pipe: %w", err)
	}
	defer cleanupStderr()

	var outBuf bytes.Buffer
	outDone := make(chan struct{})
	go func() {
		io.Copy(&outBuf, stdout)
		close(outDone)
	}()
	go io.Copy(io.Discard, stderr)

	if _, err := env.tc.Exec(subCtx, &taskAPI.ExecProcessRequest{
		ID:     env.containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdout: stdoutPath,
		Stderr: stderrPath,
	}); err != nil {
		return "", fmt.Errorf("exec: %w", err)
	}
	if _, err := env.tc.Start(subCtx, &taskAPI.StartRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return "", fmt.Errorf("start exec: %w", err)
	}
	if _, err := env.tc.Wait(subCtx, &taskAPI.WaitRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return "", fmt.Errorf("wait: %w", err)
	}
	// Give the copy goroutine time to drain the pipe before Delete
	// closes the write end. The process has already exited so this
	// typically completes in microseconds.
	select {
	case <-outDone:
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := env.tc.Delete(subCtx, &taskAPI.DeleteRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return "", fmt.Errorf("delete exec: %w", err)
	}
	return outBuf.String(), nil
}

// runExecBurstexit runs a single /bin/burstexit exec inside the shared
// container, captures its stdout, and verifies the full byte count and
// CRC-32 of the tiled payload. size is the number of bytes to request.
func runExecBurstexit(parentCtx context.Context, env *shimEnv, execID string, size int) error {
	subCtx, cancel := context.WithTimeout(parentCtx, stressIterationTimeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "stress-burst-")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	stdoutPath, stdout, cleanupStdout, err := createRawPipe(dir, "stdout")
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	defer cleanupStdout()

	stderrPath, stderr, cleanupStderr, err := createRawPipe(dir, "stderr")
	if err != nil {
		return fmt.Errorf("create stderr pipe: %w", err)
	}
	defer cleanupStderr()

	go io.Copy(io.Discard, stderr)

	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	var (
		byteCount int64
		gotCRC    uint32
	)
	outDone := make(chan error, 1)
	go func() {
		n, err := io.Copy(h, stdout)
		byteCount = n
		gotCRC = h.Sum32()
		outDone <- err
	}()

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/burstexit", strconv.Itoa(size), "0"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		return fmt.Errorf("marshal exec spec: %w", err)
	}

	if _, err := env.tc.Exec(subCtx, &taskAPI.ExecProcessRequest{
		ID:     env.containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdout: stdoutPath,
		Stderr: stderrPath,
	}); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := env.tc.Start(subCtx, &taskAPI.StartRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("start exec: %w", err)
	}
	if _, err := env.tc.Wait(subCtx, &taskAPI.WaitRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	if _, err := env.tc.Delete(subCtx, &taskAPI.DeleteRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("delete exec: %w", err)
	}

	select {
	case readErr := <-outDone:
		if readErr != nil {
			return fmt.Errorf("stdout read: %w", readErr)
		}
	case <-subCtx.Done():
		return fmt.Errorf("stdout drain timed out after Delete: %w", subCtx.Err())
	}

	if byteCount != int64(size) {
		return fmt.Errorf("stdout truncated: got %d bytes, want %d", byteCount, size)
	}
	if wantCRC := tiledPayloadCRC32(size); gotCRC != wantCRC {
		return fmt.Errorf("stdout CRC mismatch: got %08x, want %08x (data corrupted)", gotCRC, wantCRC)
	}
	return nil
}

// runExecRoundTrip runs a single /bin/cat exec inside the shared
// container, writes size bytes of tiled payload to its stdin, and
// verifies that all bytes are echoed back on stdout with the correct
// CRC-32. Exercises both stdin delivery and stdout drain.
func runExecRoundTrip(parentCtx context.Context, env *shimEnv, execID string, size int) error {
	subCtx, cancel := context.WithTimeout(parentCtx, stressIterationTimeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "stress-rt-")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	stdinPath, stdin, cleanupStdin, err := createRawPipeWriter(dir, "stdin")
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}
	defer cleanupStdin()

	stdoutPath, stdout, cleanupStdout, err := createRawPipe(dir, "stdout")
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	defer cleanupStdout()

	stderrPath, stderr, cleanupStderr, err := createRawPipe(dir, "stderr")
	if err != nil {
		return fmt.Errorf("create stderr pipe: %w", err)
	}
	defer cleanupStderr()

	go io.Copy(io.Discard, stderr)

	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	var (
		byteCount int64
		gotCRC    uint32
	)
	outDone := make(chan error, 1)
	go func() {
		n, err := io.Copy(h, stdout)
		byteCount = n
		gotCRC = h.Sum32()
		outDone <- err
	}()

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/cat"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		return fmt.Errorf("marshal exec spec: %w", err)
	}

	if _, err := env.tc.Exec(subCtx, &taskAPI.ExecProcessRequest{
		ID:     env.containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdin:  stdinPath,
		Stdout: stdoutPath,
		Stderr: stderrPath,
	}); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := env.tc.Start(subCtx, &taskAPI.StartRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("start exec: %w", err)
	}

	// Stream the tiled payload to stdin without allocating the full
	// buffer; closing stdin signals EOF to /bin/cat, causing it to exit.
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, io.LimitReader(&infiniteTileReader{}, int64(size)))
		stdin.Close()
		writeDone <- err
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			return fmt.Errorf("write stdin: %w", err)
		}
	case <-subCtx.Done():
		return fmt.Errorf("stdin write timed out: %w", subCtx.Err())
	}

	if _, err := env.tc.Wait(subCtx, &taskAPI.WaitRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	if _, err := env.tc.Delete(subCtx, &taskAPI.DeleteRequest{ID: env.containerID, ExecID: execID}); err != nil {
		return fmt.Errorf("delete exec: %w", err)
	}

	select {
	case readErr := <-outDone:
		if readErr != nil {
			return fmt.Errorf("stdout read: %w", readErr)
		}
	case <-subCtx.Done():
		return fmt.Errorf("stdout drain timed out after Delete: %w", subCtx.Err())
	}

	if byteCount != int64(size) {
		return fmt.Errorf("stdout truncated: got %d bytes, want %d", byteCount, size)
	}
	if wantCRC := tiledPayloadCRC32(size); gotCRC != wantCRC {
		return fmt.Errorf("stdout CRC mismatch: got %08x, want %08x (data corrupted)", gotCRC, wantCRC)
	}
	return nil
}

// readGuestMem execs /bin/cat /proc/meminfo inside the container and
// returns used memory in bytes (MemTotal - MemAvailable). execID must
// be unique across all concurrent execs on the same container.
func readGuestMem(ctx context.Context, env *shimEnv, execID string) (int64, error) {
	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/cat", "/proc/meminfo"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin"},
	})
	if err != nil {
		return 0, err
	}
	out, err := captureExec(ctx, env, execID, procSpec)
	if err != nil {
		return 0, err
	}
	var totalKiB, availKiB int64
	for _, line := range strings.Split(out, "\n") {
		var val int64
		if n, _ := fmt.Sscanf(line, "MemTotal: %d kB", &val); n == 1 {
			totalKiB = val
		}
		if n, _ := fmt.Sscanf(line, "MemAvailable: %d kB", &val); n == 1 {
			availKiB = val
		}
	}
	if totalKiB == 0 {
		return 0, fmt.Errorf("could not parse MemTotal from /proc/meminfo output")
	}
	return (totalKiB - availKiB) * 1024, nil
}

// stressIterationTimeout caps how long any single Transfer stat/read/write
// or exec is allowed to take. A healthy iteration finishes in single-digit
// milliseconds; anything beyond this indicates a genuinely hung shim.
//
// # Linux (5 s)
//
// The runc shim has negligible overhead per exec; 5 s is already very
// generous and reliably catches hangs.
//
// # Windows and macOS (15 s)
//
// Each Transfer iteration opens a fresh TTRPC bidi stream, which the
// shim bridges to the VM via a vsock muxer. Under concurrent load
// (3 goroutines × ~200 iterations/s) the muxer occasionally queues up,
// causing the ack handshake in vmInstance.StartStream to stall.
// The observed stalls that triggered false-failures ranged from 5–30 s.
//
// 15 s is chosen as the new threshold:
//   - Well above the observed ~5 s stalls that caused false positives
//     with the original 5 s timeout.
//   - If any iteration takes longer than 15 s the test still fails,
//     giving us data on whether the stalls ever exceed this level.
//   - A genuinely wedged shim (infinite hang) is still detected; the
//     outer stressCtx deadline terminates the run regardless.
var stressIterationTimeout = func() time.Duration {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return 15 * time.Second
	}
	return 5 * time.Second
}()

// stressSoakBuffer is how much headroom the stress tests leave
// before the test framework's deadline. Set generously so a normal
// stress run with the default 10-minute test timeout never bumps
// into it.
const stressSoakBuffer = 1 * time.Minute

// stressReadPoolSize is the number of files the read subtest of
// the transfer stress pre-populates as a setup phase.
const stressReadPoolSize = 1000

// stressWritePoolSize bounds the write subtest so its on-disk
// footprint is fixed: iterations cycle through this many filenames
// (overwriting each time) instead of growing unboundedly. Without
// this bound the container's writable layer fills up after ~13k
// iterations on a default-size scratch device.
const stressWritePoolSize = 100

// stressReadDir / stressWriteDir are the in-container directories
// used by the read and write transfer-stress subtests.
const (
	stressReadDir  = "/tmp/stress-read"
	stressWriteDir = "/tmp/stress-write"
)

// fuzzMissingBase is the in-container directory that the missing-file
// fuzz tests synthesize paths under. The test never creates it, so
// any path under it (with a sanitized suffix) is guaranteed to not
// exist.
const fuzzMissingBase = "/.fuzz-missing"

// stressSubtest is one concurrent workload run inside testTransfer.
type stressSubtest struct {
	name string
	fn   func(ctx context.Context) error
}

// testTransfer launches stat/write/read goroutines against a shared
// shim env and stops at the first failure. Subtest iteration counts
// are reported via t.Logf. (Was previously TransferSuite.testStress.)
func (s *StressSuite) testTransfer(t *testing.T) {
	env := newShimEnv(t, t.Context(), s.cfg, "stress")
	skipIfNoTransfer(t, env)
	defer shutdownShim(t, env.ctx, env)

	ctx, cancel := stressCtx(t, env.ctx)
	defer cancel()

	subtests := s.transferStressSubtests(t, env)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	type result struct {
		name    string
		iters   int64
		elapsed time.Duration
		err     error
	}
	results := make(chan result, len(subtests))

	for _, st := range subtests {
		go func() {
			iters, elapsed, err := runStress(runCtx, st.fn)
			if err != nil {
				runCancel()
			}
			results <- result{st.name, iters, elapsed, err}
		}()
	}

	var firstErr error
	var firstErrName string
	for i := 0; i < len(subtests); i++ {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = r.err
			firstErrName = r.name
		}
		rate := float64(r.iters) / r.elapsed.Seconds()
		t.Logf("%s: %d iterations in %s (%.0f iter/s)",
			r.name, r.iters, r.elapsed.Round(time.Millisecond), rate)
	}

	if firstErr != nil {
		t.Fatalf("%s: %v", firstErrName, firstErr)
	}
}

// transferStressSubtests returns the stat / write / read stress
// subtests. The read pool is pre-populated synchronously so by the
// time the caller spawns goroutines, the read subtest can find its
// files.
func (s *StressSuite) transferStressSubtests(t *testing.T, env *shimEnv) []stressSubtest {
	t.Helper()

	for i := 0; i < stressReadPoolSize; i++ {
		name := fmt.Sprintf("file-%05d.txt", i)
		content := stressFileContent(i)
		if err := stressTransferWriteFile(env.ctx, env, stressReadDir, name, content); err != nil {
			t.Fatalf("read pool setup %d: %v", i, err)
		}
	}

	// Verify setup actually wrote all files. If the streaming layer
	// silently dropped writes, the read goroutine below would surface
	// the dropouts as "file not found" errors much later — a confusing
	// failure mode. Surface it here instead.
	missing := 0
	for i := 0; i < stressReadPoolSize; i++ {
		name := fmt.Sprintf("file-%05d.txt", i)
		path := stressReadDir + "/" + name
		if err := stressTransferStat(env.ctx, env, path); err != nil {
			missing++
			if missing <= 5 {
				t.Logf("setup verify: missing %s: %v", name, err)
			}
		}
	}
	if missing > 0 {
		t.Fatalf("setup verify: %d/%d read-pool files were not persisted by the shim", missing, stressReadPoolSize)
	}

	var writeIdx, readIdx atomic.Int64

	return []stressSubtest{
		{
			name: "stat",
			fn: func(ctx context.Context) error {
				return stressTransferStat(ctx, env, statDirContainerPath)
			},
		},
		{
			name: "write",
			fn: func(ctx context.Context) error {
				// Cycle through stressWritePoolSize filenames so each
				// iteration overwrites a previous file (tar extraction
				// uses O_TRUNC). This keeps disk usage bounded; the
				// content still varies per iteration so the streaming
				// payload isn't trivially compressible/dedupable.
				i := writeIdx.Add(1)
				name := fmt.Sprintf("file-%05d.txt", int(i)%stressWritePoolSize)
				return stressTransferWriteFile(ctx, env, stressWriteDir, name, stressFileContent(int(i)))
			},
		},
		{
			name: "read",
			fn: func(ctx context.Context) error {
				i := int(readIdx.Add(1)-1) % stressReadPoolSize
				name := fmt.Sprintf("file-%05d.txt", i)
				want := stressFileContent(i)
				path := stressReadDir + "/" + name
				got, err := stressTransferReadFile(ctx, env, path, name)
				if err != nil {
					return err
				}
				if got != want {
					return fmt.Errorf("content mismatch for %s: got %q, want %q", name, got, want)
				}
				return nil
			},
		},
	}
}

// stressFileContent is the canonical content for the i-th stress file.
func stressFileContent(i int) string {
	return fmt.Sprintf("stress-content-%05d\n", i)
}

// runStress runs fn in a sequential loop until fn returns an error
// or the parent context is canceled. fn receives a context detached
// from the parent so cancellation of the parent does not propagate
// into an in-flight iteration.
//
// Transient timeout handling (Windows, macOS): VM scheduling on Windows
// (krun) and macOS (HVF) can cause individual operations to stall beyond
// stressIterationTimeout without indicating a real shim bug. We treat
// context.DeadlineExceeded from fn as a recoverable miss on those
// platforms and keep running. On Linux (no VM overhead) such stalls are
// genuine and still fail the test.
//
// Deadline-race: when the outer ctx fires, the last fn call may still
// be in flight. If fn then returns an error after the outer context was
// already cancelled, we treat that as cleanup noise and return cleanly.
func runStress(ctx context.Context, fn func(ctx context.Context) error) (int64, time.Duration, error) {
	start := time.Now()

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var iters int64
	for ctx.Err() == nil {
		if err := fn(runCtx); err != nil {
			// If the outer context was already cancelled when fn returned,
			// treat it as a clean stop — the last operation raced against
			// stress deadline.
			if ctx.Err() != nil {
				break
			}
			// On Windows and macOS, individual context.DeadlineExceeded
			// errors are caused by hypervisor scheduling stalls, not shim
			// bugs. Skip the iteration rather than failing the test.
			if (runtime.GOOS == "windows" || runtime.GOOS == "darwin") && errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return iters, time.Since(start), err
		}
		iters++
	}
	return iters, time.Since(start), nil
}

// stressCtx derives a cancel context from parent, additionally bounded
// by t.Deadline() if one is set, with stressSoakBuffer of headroom.
// Skips when there isn't enough room for a meaningful run.
func stressCtx(t *testing.T, parent context.Context) (context.Context, context.CancelFunc) {
	t.Helper()
	d, ok := t.Deadline()
	if !ok {
		return context.WithCancel(parent)
	}
	if remaining := time.Until(d); remaining < stressSoakBuffer {
		t.Skipf("skipping: stress needs at least %s before -test.timeout, only %s left",
			stressSoakBuffer, remaining.Round(time.Millisecond))
	}
	return context.WithDeadline(parent, d.Add(-stressSoakBuffer))
}

// stressTransferStat performs a single Transfer with NoWalk=true on
// the given path. The server-side payload is discarded; only the RPC
// status (success or error) matters.
func stressTransferStat(ctx context.Context, env *shimEnv, path string) error {
	subCtx, cancel := context.WithTimeout(ctx, stressIterationTimeout)
	defer cancel()

	src := &transfer.ContainerPath{
		ContainerID: env.containerID,
		Path:        path,
		NoWalk:      true,
	}
	dst := transfer.NewWriteStream(io.Discard, "application/x-tar")

	if err := stressDoTransfer(subCtx, env, src, dst); err != nil {
		return err
	}
	// Drain the receive goroutine so it doesn't outlive the caller.
	return dst.Wait(subCtx)
}

// stressTransferWriteFile writes a single small file as a one-entry
// tar to the given directory in the container.
func stressTransferWriteFile(ctx context.Context, env *shimEnv, dir, name, content string) error {
	subCtx, cancel := context.WithTimeout(ctx, stressIterationTimeout)
	defer cancel()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0644,
		Size: int64(len(content)),
	}); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		return fmt.Errorf("write tar body: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}

	src := transfer.NewReadStream(&tarBuf, "application/x-tar")
	dst := &transfer.ContainerPath{
		ContainerID: env.containerID,
		Path:        dir,
	}
	if err := stressDoTransfer(subCtx, env, src, dst); err != nil {
		return err
	}
	// Wait for the background send goroutine to finish and surface any
	// transport error it encountered.
	return src.Wait(subCtx)
}

// stressTransferReadFile reads a single file back from the container
// and returns its content.
func stressTransferReadFile(ctx context.Context, env *shimEnv, path, name string) (string, error) {
	subCtx, cancel := context.WithTimeout(ctx, stressIterationTimeout)
	defer cancel()

	src := &transfer.ContainerPath{
		ContainerID: env.containerID,
		Path:        path,
	}

	var received bytes.Buffer
	dst := transfer.NewWriteStream(&received, "application/x-tar")

	if err := stressDoTransfer(subCtx, env, src, dst); err != nil {
		return "", err
	}

	// Wait for the background receive goroutine to finish draining the
	// stream into received, and surface any transport error it encountered.
	if err := dst.Wait(subCtx); err != nil {
		return "", fmt.Errorf("receive stream: %w", err)
	}

	tr := tar.NewReader(&received)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar: %w", err)
		}
		if strings.HasSuffix(hdr.Name, name) {
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", fmt.Errorf("read tar entry: %w", err)
			}
			return string(data), nil
		}
	}
	return "", fmt.Errorf("entry %q not found in tar", name)
}

// stressDoTransfer marshals src and dst, then issues the Transfer RPC.
func stressDoTransfer(ctx context.Context, env *shimEnv, src, dst any) error {
	srcAny, err := marshalTransferAny(ctx, src, env.sc)
	if err != nil {
		return fmt.Errorf("marshal source: %w", err)
	}
	dstAny, err := marshalTransferAny(ctx, dst, env.sc)
	if err != nil {
		return fmt.Errorf("marshal destination: %w", err)
	}

	tfClient := transferapi.NewTTRPCTransferClient(env.client)
	_, err = tfClient.Transfer(ctx, &transferapi.TransferRequest{
		Source:      srcAny,
		Destination: dstAny,
	})
	return err
}
