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
	tasktypes "github.com/containerd/containerd/api/types/task"
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
	t.Run("LargeStdioRoundTrip", s.testLargeStdioRoundTrip)
	t.Run("Clock", s.testClock)
	t.Run("ExitCodes", s.testExitCodes)
	t.Run("LargeFileRead", s.testLargeFileRead)
	t.Run("BindMountRead", s.testBindMountRead)
	t.Run("FastExitOutput", s.testFastExitOutput)
	t.Run("ExecOutputDrainAfterExit", s.testExecOutputDrainAfterExit)
	t.Run("ExecDiscardIO", s.testExecDiscardIO)
	t.Run("ExecCommandNotFound", s.testExecCommandNotFound)
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

// testLargeStdioRoundTrip pipes 20 MiB through stdin → /bin/cat →
// stdout and verifies the full byte count and CRC-32 checksum on the
// way out. It uses the same deterministic 0x00..0xff tile payload as
// the FastExit tests.
//
// This test asserts that a conforming shim delivers all bytes written
// to exec stdin to the process and relays all bytes written by the
// process to stdout back to the host without truncation under sustained
// load. Two API contracts are verified:
//
//   - All stdin bytes must be forwarded to the process before the stdin
//     connection is closed. A shim that drops buffered bytes on the
//     stdin close path will produce truncated output from cat.
//
//   - All stdout bytes produced by the process must be delivered to the
//     host before the stdout connection is closed. A shim that drops
//     buffered bytes when the process exits will produce truncated
//     output.
//
// Stdin is closed via the CloseIO RPC, not by simply closing the
// client-side FIFO write end. This matches how containerd itself signals
// EOF: the shim holds its own write-end reference on the stdin FIFO (to
// unblock its internal O_RDONLY open) and only releases it when it
// receives a CloseIO request. Closing the test's write end alone leaves
// the shim's reference open, so cat never sees EOF. The CloseIO RPC
// instructs the shim to drop its reference, delivering EOF to the
// process — exactly the protocol used by `ctr exec`.
//
// A third API contract is also verified: the shim must close the exec's
// stdout connection once the process exits and its output has been
// flushed, without waiting for the caller to issue Delete. The test
// waits for stdout EOF before calling Delete; a shim that defers the
// stdout close until Delete will time out here.
//
// Both failure modes are detected by the byte-count and CRC-32
// assertions at the end of the test.
func (s *ExecSuite) testLargeStdioRoundTrip(t *testing.T) {
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

	execID := "large-rt"
	execDir := t.TempDir()
	execStdin, execStdout, execStderr := createStdioFifos(t, execDir)

	stdoutReader, err := openPipeReader(ctx, execStdout)
	if err != nil {
		t.Fatal("open stdout reader:", err)
	}
	t.Cleanup(func() { stdoutReader.Close() })

	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	var byteCount int64
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		byteCount, _ = io.Copy(h, stdoutReader)
	}()

	drainFifo(t, ctx, execStderr)

	stdinFifo, err := openPipeWriter(ctx, execStdin)
	if err != nil {
		t.Fatal("open stdin pipe:", err)
	}

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

	// Write the tiled payload to stdin in a background goroutine.
	// The write and the stdout drain run concurrently so that neither
	// the stdin FIFO nor cat's stdout pipe can fill and deadlock.
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdinFifo, io.LimitReader(&infiniteTileReader{}, int64(largeStdioPayloadSize)))
		stdinFifo.Close()
		writeDone <- err
	}()

	// Wait for all stdin to be written.
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal("write to stdin failed:", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("timed out writing stdin payload")
	}

	// Signal EOF to the in-container process via the CloseIO RPC.
	//
	// Closing the client-side FIFO write end alone is not sufficient:
	// the shim holds its own write-end reference on the stdin FIFO
	// (opened in openStdin to unblock the shim's internal O_RDONLY
	// open) and releases it only when it receives a CloseIO request.
	// This matches the protocol used by containerd and `ctr exec`:
	// the client calls CloseIO after it is done writing stdin, which
	// causes the shim to close its write reference and deliver EOF to
	// the process.
	if _, err := tc.CloseIO(ctx, &taskAPI.CloseIORequest{
		ID:     containerID,
		ExecID: execID,
		Stdin:  true,
	}); err != nil {
		t.Fatal("CloseIO failed:", err)
	}

	// Wait for cat to exit (it exits once stdin reaches EOF).
	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID, ExecID: execID})
	if err != nil {
		t.Fatal("exec wait failed:", err)
	}
	if waitResp.ExitStatus != 0 {
		t.Fatalf("cat exited with status %d", waitResp.ExitStatus)
	}

	// Wait for the stdout reader to reach EOF before calling Delete.
	//
	// A conforming shim must close the exec's stdout connection once
	// the process exits and its output has been flushed — it must not
	// defer closing stdout until Delete is called. Waiting here before
	// Delete tests that contract: if the shim closes stdout promptly on
	// process exit, outDone fires quickly; if the shim holds stdout
	// open until Delete, this will time out.
	select {
	case <-outDone:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for stdout drain after process exit")
	}

	if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID, ExecID: execID}); err != nil {
		t.Fatal("exec delete failed:", err)
	}

	if byteCount != int64(largeStdioPayloadSize) {
		t.Fatalf("got %d bytes, want %d (truncated by %d bytes)",
			byteCount, largeStdioPayloadSize, int64(largeStdioPayloadSize)-byteCount)
	}
	gotCRC := h.Sum32()
	if wantCRC := tiledPayloadCRC32(largeStdioPayloadSize); gotCRC != wantCRC {
		t.Fatalf("CRC mismatch: got %08x, want %08x (data corrupted)", gotCRC, wantCRC)
	}
	t.Logf("ok — %d bytes, CRC %08x", byteCount, gotCRC)

	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: containerID})
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

// largeStdioPayloadSize is the number of bytes piped through the
// LargeStdioRoundTrip test. Larger than burstPayloadSize to stress the
// shim's in-flight buffering more aggressively.
const largeStdioPayloadSize = 20 * 1024 * 1024

// tiledPayload builds a repeating 0x00..0xff tiled payload of the given
// size. This matches the byte stream produced by /bin/burstexit and is
// shared between the exec and stress suite tests.
func tiledPayload(size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	return payload
}

// tiledPayloadCRC32 returns the Castagnoli CRC-32 of a tiled payload of
// the given size without allocating the full payload. Used when only the
// expected checksum is needed (e.g. verifying /bin/burstexit output).
func tiledPayloadCRC32(size int) uint32 {
	var tile [256]byte
	for i := range tile {
		tile[i] = byte(i)
	}
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	for remaining := size; remaining > 0; {
		n := remaining
		if n > len(tile) {
			n = len(tile)
		}
		h.Write(tile[:n])
		remaining -= n
	}
	return h.Sum32()
}

// burstExpectedCRC32 returns the Castagnoli CRC-32 of the deterministic
// tiled payload of exactly burstPayloadSize bytes.
func burstExpectedCRC32() uint32 {
	return tiledPayloadCRC32(burstPayloadSize)
}

// infiniteTileReader emits an infinite repeating 0x00..0xff tile stream.
// Wrap with io.LimitReader to produce a payload of exactly n bytes without
// allocating the full buffer.
type infiniteTileReader struct{ off int64 }

func (r *infiniteTileReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte((r.off + int64(i)) % 256)
	}
	r.off += int64(len(p))
	return len(p), nil
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

// testExecOutputDrainAfterExit verifies that a conforming shim promptly
// closes the exec process's stdout connection when the process exits,
// allowing Shutdown to complete without waiting for the 30 s ioShutdown
// fallback.
//
// The init container is started with null (empty) stdout/stderr so that
// init's stdio shutdown completes instantly and does not consume the
// shared 30 s shutdown deadline. This isolates the exec's stdio close
// as the sole determinant of total Shutdown duration.
//
// A non-conforming shim that fails to propagate the write-end close of
// the exec's stdout back to the host will leave the host copy goroutine
// blocked until the 30 s fallback fires; Shutdown will exceed the 5 s
// assertion and the test will fail on the duration check.
func (s *ExecSuite) testExecOutputDrainAfterExit(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg)

	// Start the init container with null stdout/stderr so its stdio
	// shutdown completes instantly. If we used real FIFOs, /bin/forever
	// (which never writes) would hold stdout open, consuming the shared
	// 30 s shutdown deadline before exec's ioShutdown even starts.
	ns := uniqueTestNamespace(t, "exec")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, "", "", rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID}); err != nil {
		t.Fatal("start failed:", err)
	}

	execID := "drain-0"
	execDir := t.TempDir()
	execStdout, execStderr := createIOFifos(t, execDir)

	var execBuf bytes.Buffer
	var execMu sync.Mutex
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

	// Time the Shutdown call. With null init IO, the only remaining wait
	// is exec's stdio shutdown — which is the variable under test. A
	// conforming shim closes the exec's stdout connection promptly when
	// the process exits; ioDone fires in milliseconds and Shutdown
	// returns well within the 5 s bound.
	shutdownStart := time.Now()
	tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: containerID})
	elapsed := time.Since(shutdownStart)

	// 5 s is well above sub-second propagation on a correct shim and well
	// below the 30 s shared shutdown-context deadline.
	const maxShutdownDuration = 5 * time.Second
	if elapsed > maxShutdownDuration {
		t.Fatalf("Shutdown blocked for %v (> %v): shim did not propagate exec stdout close — ioShutdown 30s fallback is masking the root cause",
			elapsed.Round(time.Millisecond), maxShutdownDuration)
	}

	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stdout FIFO did not drain within 2s after Shutdown returned")
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

// testExecDiscardIO execs a short-lived process with all stdio discarded
// (empty Stdin/Stdout/Stderr), waits for it to exit, then Deletes the exec
// while the container's init keeps running.
func (s *ExecSuite) testExecDiscardIO(t *testing.T) {
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

	execID := "discard-io"

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/echo", "discardme"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal("failed to marshal exec spec:", err)
	}

	// Deliberately leave Stdin/Stdout/Stderr unset: the exec has no I/O to
	// forward, so the shim's forwardIO returns a nil shutdown func.
	if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
		ID:     containerID,
		ExecID: execID,
		Spec:   procSpec,
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
	t.Log("discard-io exec exit status:", waitResp.ExitStatus)

	if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID, ExecID: execID}); err != nil {
		t.Fatal("exec delete failed (shim likely crashed on a nil IO shutdown):", err)
	}

	stateResp, err := tc.State(ctx, &taskAPI.StateRequest{ID: containerID})
	if err != nil {
		t.Fatal("container State after exec delete failed (shim likely crashed):", err)
	}
	if stateResp.Status != tasktypes.Status_RUNNING {
		t.Fatalf("container is %s after deleting a discarded-IO exec; want RUNNING (shim likely crashed)", stateResp.Status)
	}

	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}

// testExecCommandNotFound execs a binary that does not exist in the rootfs and
// verifies two things: the exec start is reported as a failure, and —
// critically — that deleting the failed exec returns promptly instead of
// blocking on the 30 s ioShutdown fallback.
//
// A conforming shim closes the exec's stream connections on the start-failure
// path, so the caller sees EOF immediately and Delete returns in milliseconds.
func (s *ExecSuite) testExecCommandNotFound(t *testing.T) {
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

	execID := "notfound-0"

	execStdout, execStderr := createIOFifos(t, t.TempDir())
	drainFifo(t, ctx, execStdout)
	drainFifo(t, ctx, execStderr)

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/this-binary-does-not-exist"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal("failed to marshal exec spec:", err)
	}

	// Exec (create) succeeds: the shim sets up IO forwarding and registers the
	// exec. The failure happens at Start, when the runtime tries to exec the
	// missing binary.
	if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
		ID:     containerID,
		ExecID: execID,
		Spec:   procSpec,
		Stdout: execStdout,
		Stderr: execStderr,
	}); err != nil {
		t.Fatal("exec failed:", err)
	}

	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID, ExecID: execID}); err == nil {
		t.Fatal("expected exec start of a missing binary to fail, got nil error")
	} else {
		t.Log("exec start failed as expected:", err)
	}

	// The regression assertion: deleting the failed exec must not block on the
	// 30 s ioShutdown fallback. 5 s is far above sub-second cleanup on a
	// conforming shim and far below the 30 s fallback.
	const maxDeleteDuration = 5 * time.Second
	deleteStart := time.Now()
	_, delErr := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID, ExecID: execID})
	elapsed := time.Since(deleteStart)
	if elapsed > maxDeleteDuration {
		t.Fatalf("exec Delete blocked for %v (> %v): shim did not close the exec stream connections on the start-failure path — ioShutdown 30s fallback is masking the leak",
			elapsed.Round(time.Millisecond), maxDeleteDuration)
	}
	// A Delete error is not the property under test (the exec already failed to
	// start); only the absence of the 30 s stall is. Log it for context.
	if delErr != nil {
		t.Log("exec delete returned:", delErr)
	}

	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}
