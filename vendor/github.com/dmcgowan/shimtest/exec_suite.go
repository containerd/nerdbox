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
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// ExecSuite contains tests gated on the "exec" feature: running
// processes inside a container, stdio round-trip, the in-VM clock,
// exec exit-code propagation, and file-IO benchmarks driven via
// hashverify.
type ExecSuite struct {
	cfg Config
	
}

// NewExecSuite constructs an ExecSuite from the given options.
func NewExecSuite(cfg Config) *ExecSuite {
	return &ExecSuite{cfg: cfg}
}

// Run runs every test in the suite as a subtest of t.
func (s *ExecSuite) Run(t *testing.T) {
	t.Helper()
	registerShimLeakCheck(t, s.cfg.ShimBinary)
	t.Run("Exec", s.testExec)
	t.Run("StdioRoundTrip", s.testStdioRoundTrip)
	t.Run("Clock", s.testClock)
	t.Run("ExitCodes", s.testExitCodes)
	t.Run("LargeFileRead", s.testLargeFileRead)
	t.Run("BindMountRead", s.testBindMountRead)
	t.Run("FastExitOutput", s.testFastExitOutput)
}

// TestExec runs `echo execworks` inside a container and verifies the
// stdout makes it back through the shim.
func (s *ExecSuite) testExec(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "exec")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	drainFifo(t, ctx, stdoutPath)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID}); err != nil {
		t.Fatal("start failed:", err)
	}

	execID := "exec1"
	execStdout, execStderr := createIOFifos(t, t.TempDir())

	var execBuf bytes.Buffer
	var execMu sync.Mutex
	drainFifoInto(t, ctx, execStdout, &execBuf, &execMu)
	drainFifo(t, ctx, execStderr)

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/echo", "execworks"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal("failed to marshal exec spec:", err)
	}

	if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
		ID:     containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdout: execStdout,
		Stderr: execStderr,
	}); err != nil {
		t.Fatal("exec failed:", err)
	}

	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID, ExecID: execID}); err != nil {
		t.Fatal("exec start failed:", err)
	}

	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID, ExecID: execID})
	if err != nil {
		t.Fatal("exec wait failed:", err)
	}
	t.Log("exec exit status:", waitResp.ExitStatus)

	time.Sleep(200 * time.Millisecond)

	execMu.Lock()
	got := execBuf.String()
	execMu.Unlock()
	t.Log("exec stdout:", strings.TrimSpace(got))

	if !strings.Contains(got, "execworks") {
		t.Fatalf("exec output %q does not contain 'execworks'", got)
	}

	if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID, ExecID: execID}); err != nil {
		t.Fatal("exec delete failed:", err)
	}

	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}

// TestStdioRoundTrip writes a token to stdin of an exec'd `cat` and
// verifies the same token is read back from stdout.
func (s *ExecSuite) testStdioRoundTrip(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "exec")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	drainFifo(t, ctx, stdoutPath)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID}); err != nil {
		t.Fatal("start failed:", err)
	}

	execID := "stdio-rt"
	execDir := t.TempDir()
	execStdin, execStdout, execStderr := createStdioFifos(t, execDir)

	var execBuf bytes.Buffer
	var execMu sync.Mutex
	drainFifoInto(t, ctx, execStdout, &execBuf, &execMu)
	drainFifo(t, ctx, execStderr)

	stdinFifo, err := openPipeWriter(ctx, execStdin)
	if err != nil {
		t.Fatal("open stdin pipe:", err)
	}
	defer stdinFifo.Close()

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/cat"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal("marshal exec spec:", err)
	}

	if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
		ID:     containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdin:  execStdin,
		Stdout: execStdout,
		Stderr: execStderr,
	}); err != nil {
		t.Fatal("exec failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID, ExecID: execID}); err != nil {
		t.Fatal("exec start failed:", err)
	}

	token := randomSuffix() + "\n"
	if _, err := stdinFifo.Write([]byte(token)); err != nil {
		t.Fatal("write to stdin:", err)
	}

	deadline := time.After(30 * time.Second)
	for {
		execMu.Lock()
		got := execBuf.String()
		execMu.Unlock()
		if strings.Contains(got, strings.TrimSpace(token)) {
			t.Log("stdio round trip succeeded, got:", strings.TrimSpace(got))
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for stdio round trip, got: %q", got)
		case <-time.After(10 * time.Millisecond):
		}
	}

	stdinFifo.Close()
	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, ExecID: execID, Signal: uint32(syscall.SIGKILL)})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID, ExecID: execID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID, ExecID: execID})
	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}

// TestClock verifies the in-container clock matches the host's
// (within a tolerance) by running `date +%s` and bracketing it with
// host timestamps.
func (s *ExecSuite) testClock(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "exec")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	drainFifo(t, ctx, stdoutPath)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID}); err != nil {
		t.Fatal("start failed:", err)
	}

	execID := "clock1"
	execStdout, execStderr := createIOFifos(t, t.TempDir())

	var execBuf bytes.Buffer
	var execMu sync.Mutex
	drainFifoInto(t, ctx, execStdout, &execBuf, &execMu)
	drainFifo(t, ctx, execStderr)

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/date", "+%s"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin:/sbin:/usr/sbin"},
	})
	if err != nil {
		t.Fatal("failed to marshal exec spec:", err)
	}

	before := time.Now()
	if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
		ID:     containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdout: execStdout,
		Stderr: execStderr,
	}); err != nil {
		t.Fatal("exec failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID, ExecID: execID}); err != nil {
		t.Fatal("exec start failed:", err)
	}

	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID, ExecID: execID})
	if err != nil {
		t.Fatal("exec wait failed:", err)
	}
	after := time.Now()

	if waitResp.ExitStatus != 0 {
		t.Fatalf("clock exec exited with status %d", waitResp.ExitStatus)
	}
	time.Sleep(200 * time.Millisecond)

	execMu.Lock()
	got := strings.TrimSpace(execBuf.String())
	execMu.Unlock()
	t.Log("clock exec output:", got)

	vmEpoch, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("failed to parse VM timestamp %q: %v", got, err)
	}

	vmTime := time.Unix(vmEpoch, 0)
	t.Logf("host before: %s", before.UTC())
	t.Logf("VM time:     %s (epoch %d)", vmTime.UTC(), vmEpoch)
	t.Logf("host after:  %s", after.UTC())

	tolerance := 2 * time.Second
	if vmTime.Before(before.Add(-tolerance)) || vmTime.After(after.Add(tolerance)) {
		t.Fatalf("VM clock is not synchronized: VM=%s, host range=[%s, %s], drift=%s",
			vmTime.UTC(), before.UTC(), after.UTC(),
			fmt.Sprintf("%+.1fs", vmTime.Sub(before).Seconds()))
	}

	if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID, ExecID: execID}); err != nil {
		t.Fatal("exec delete failed:", err)
	}
	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}

// TestExitCodes runs /bin/exit N inside a container via exec for a
// range of values and verifies each is propagated back via Wait.
func (s *ExecSuite) testExitCodes(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "exec")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	drainFifo(t, ctx, stdoutPath)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID}); err != nil {
		t.Fatal("start failed:", err)
	}

	for i, code := range []int{0, 1, 2, 42, 127, 255} {
		t.Run(fmt.Sprintf("Exit%d", code), func(t *testing.T) {
			execID := fmt.Sprintf("exit-%d", i)
			execStdout, execStderr := createIOFifos(t, t.TempDir())
			drainFifo(t, ctx, execStdout)
			drainFifo(t, ctx, execStderr)

			procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
				Args: []string{"/bin/exit", strconv.Itoa(code)},
				Cwd:  "/",
				Env:  []string{"PATH=/bin:/usr/bin"},
			})
			if err != nil {
				t.Fatal("marshal exec spec:", err)
			}

			if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
				ID:     containerID,
				ExecID: execID,
				Spec:   procSpec,
				Stdout: execStdout,
				Stderr: execStderr,
			}); err != nil {
				t.Fatal("exec failed:", err)
			}
			if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID, ExecID: execID}); err != nil {
				t.Fatal("exec start failed:", err)
			}

			waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID, ExecID: execID})
			if err != nil {
				t.Fatal("exec wait failed:", err)
			}
			if waitResp.ExitStatus != uint32(code) {
				t.Fatalf("expected exit status %d, got %d", code, waitResp.ExitStatus)
			}
			if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID, ExecID: execID}); err != nil {
				t.Fatal("exec delete failed:", err)
			}
		})
	}

	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}

// TestLargeFileRead reads /data/bigfile (a 64 MiB fixture from the
// secondary read-only erofs layer) inside the container, verifies
// its crc32-Castagnoli, and reports throughput.
func (s *ExecSuite) testLargeFileRead(t *testing.T) {
	s.runHashverify(t, bigFileContainerPath, bigFileHashHex(), nil)
}

// TestBindMountRead streams the same 64 MiB fixture into a host
// tempfile, bind-mounts it into the container, and runs hashverify
// against the bind path.
func (s *ExecSuite) testBindMountRead(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "bigfile")
	f, err := os.Create(hostFile)
	if err != nil {
		t.Fatal("create host bigfile:", err)
	}
	if _, err := io.Copy(f, newBigFileReader()); err != nil {
		f.Close()
		t.Fatal("write host bigfile:", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal("close host bigfile:", err)
	}

	const containerPath = "/tmp/bigfile"
	mount := specs.Mount{
		Type:        "bind",
		Source:      hostFile,
		Destination: containerPath,
		Options:     []string{"rbind"},
	}
	s.runHashverify(t, containerPath, bigFileHashHex(), []specs.Mount{mount})
}

// runHashverify spins up a container with the given extra mounts,
// execs /bin/hashverify against the path, parses
// "ok bytes=N ns=M cpu_bound=K" from stdout, and reports throughput
// via t.Log.
func (s *ExecSuite) runHashverify(t *testing.T, path, hashHex string, extraMounts []specs.Mount) {
	t.Helper()
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	cid := containerID(t)

	var opts []func(*specs.Spec)
	if len(extraMounts) > 0 {
		opts = append(opts, withExtraMounts(extraMounts...))
	}
	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg, opts...)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "exec")
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

	execID := "hashv"
	execStdout, execStderr := createIOFifos(t, t.TempDir())
	var execBuf bytes.Buffer
	var execMu sync.Mutex
	drainFifoInto(t, ctx, execStdout, &execBuf, &execMu)
	drainFifo(t, ctx, execStderr)

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/hashverify", path, hashHex},
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
	if waitResp.ExitStatus != 0 {
		t.Fatalf("hashverify exit status %d", waitResp.ExitStatus)
	}

	time.Sleep(200 * time.Millisecond)

	execMu.Lock()
	out := strings.TrimSpace(execBuf.String())
	execMu.Unlock()

	var nBytes, elapsedNS, cpuBound int64
	parsed, err := fmt.Sscanf(out, "ok bytes=%d ns=%d cpu_bound=%d", &nBytes, &elapsedNS, &cpuBound)
	if err != nil || parsed != 3 {
		t.Fatalf("could not parse hashverify output %q: %v (parsed %d)", out, err, parsed)
	}

	mibPerSec := float64(nBytes) / (float64(elapsedNS) / 1e9) / (1 << 20)
	t.Logf("read %d bytes in %.2f ms = %.2f MiB/s (cpu_bound=%d)",
		nBytes, float64(elapsedNS)/1e6, mibPerSec, cpuBound)

	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid, ExecID: execID})
	tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	shutdownTask(ctx, tc, cid)
}


// burstPayloadSize is the number of bytes written by /bin/burstexit in
// the FastExitOutput and FastExitInit tests. It must be large enough
// that the kernel socket buffers cannot absorb the entire stream
// before the process exits, ensuring that the shim's host-side copy
// goroutine is still mid-read from the vsock when ioShutdown closes
// the connection.
//
// 8 MiB comfortably exceeds the default Linux socket receive buffer
// (rmem_default ≈ 208 KiB) and the 4 KiB bufPool chunk used by
// io.CopyBuffer.
const burstPayloadSize = 8 * 1024 * 1024

// burstExpectedCRC32 returns the Castagnoli CRC-32 of the deterministic
// byte stream produced by /bin/burstexit: a repeating 0x00..0xff tile
// of exactly burstPayloadSize bytes.
func burstExpectedCRC32() uint32 {
	tile := make([]byte, 256)
	for i := range tile {
		tile[i] = byte(i)
	}
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	remaining := burstPayloadSize
	for remaining > 0 {
		n := remaining
		if n > len(tile) {
			n = len(tile)
		}
		h.Write(tile[:n])
		remaining -= n
	}
	return h.Sum32()
}

// testFastExitOutput execs /bin/burstexit (writes 8 MiB then exits)
// inside a long-running container, then immediately shuts down the
// shim without waiting for the exec to finish or calling Delete.
//
// The race: the exec process writes 8 MiB quickly. The in-VM stdio
// copy goroutine drains the process's stdout pipe into the vsock
// stream. The host copy goroutine reads from the vsock into the FIFO.
// When Shutdown runs, service.shutdown → c.shutdown → ioShutdown
// closes the vsock connection. If the host goroutine is still
// mid-Read at that moment, it sees "use of closed network connection"
// and returns early, dropping whatever bytes were still buffered in
// the kernel socket receive queue.
//
// A correct shim waits for ioDone (all copy goroutines finished
// draining) before closing the connections.
func (s *ExecSuite) testFastExitOutput(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "exec")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	drainFifo(t, ctx, stdoutPath)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID}); err != nil {
		t.Fatal("start failed:", err)
	}

	execID := "burst-0"
	execDir := t.TempDir()
	execStdout, execStderr := createIOFifos(t, execDir)

	var execBuf bytes.Buffer
	var execMu sync.Mutex
	// drainFifoIntoDone closes done when the FIFO write end is closed,
	// which happens after ioShutdown returns (the copy goroutine calls
	// wc.Close after exiting). We block on this before reading execBuf.
	drainDone := drainFifoIntoDone(t, ctx, execStdout, &execBuf, &execMu)
	drainFifo(t, ctx, execStderr)

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/burstexit", strconv.Itoa(burstPayloadSize), "0"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal("marshal exec spec:", err)
	}

	if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
		ID:     containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdout: execStdout,
		Stderr: execStderr,
	}); err != nil {
		t.Fatal("exec failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID, ExecID: execID}); err != nil {
		t.Fatal("exec start failed:", err)
	}

	// Shut down the shim immediately — without waiting for the exec to
	// finish or calling Delete. This triggers service.shutdown →
	// c.shutdown → ioShutdown for the exec stream while burstexit is
	// still running (or has just exited with bytes still in the vsock
	// receive buffer). The in-VM delete+drain sequence (waitTimeout on
	// the IO wg) never runs, so the host copy goroutine races against
	// the connection close.
	//
	// We do NOT kill the init first: Kill(All:true) sends SIGKILL to
	// vminitd itself, which triggers the VM's own shutdown sequence and
	// tears down the vsock transport before the host can drain it.
	// Calling Shutdown directly keeps the VM alive long enough for
	// service.shutdown → c.shutdown → ioShutdown to drain the streams
	// before s.sb.Stop kills the VM.
	tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: containerID})

	// Wait for the FIFO reader to reach EOF. ioShutdown (via the copy
	// goroutine's wc.Close) closes the FIFO write end when the goroutine
	// exits. On a buggy shim the goroutine exits early on the close
	// error and drainDone fires before all bytes arrive.
	select {
	case <-drainDone:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for stdout drain after shutdown")
	}

	execMu.Lock()
	got := execBuf.Bytes()
	execMu.Unlock()

	wantCRC := burstExpectedCRC32()
	if len(got) != burstPayloadSize {
		t.Fatalf("got %d bytes, want %d (truncated by %d bytes)",
			len(got), burstPayloadSize, burstPayloadSize-len(got))
	}
	gotCRC := crc32.Checksum(got, crc32.MakeTable(crc32.Castagnoli))
	if gotCRC != wantCRC {
		t.Fatalf("CRC mismatch: got %08x, want %08x (data corrupted)", gotCRC, wantCRC)
	}
	t.Logf("ok — %d bytes, CRC %08x", len(got), gotCRC)
}
