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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
)

// RunSuite contains the always-run conformance tests: full container
// lifecycle, init exit-code propagation, output-then-exit, and
// event-stream verification. None of these tests are gated by a
// feature key — every shim should pass them.
type RunSuite struct {
	cfg Config
}

// NewRunSuite constructs a RunSuite from the given options.
func NewRunSuite(cfg Config) *RunSuite {
	return &RunSuite{cfg: cfg}
}

// Run runs every test in the suite as a subtest of t.
func (s *RunSuite) Run(t *testing.T) {
	t.Helper()
	registerShimLeakCheck(t, s.cfg.ShimBinary)
	t.Run("Lifecycle", s.testLifecycle)
	t.Run("InitExitCodes", s.testInitExitCodes)
	t.Run("OutputThenExit", s.testOutputThenExit)
	t.Run("Events", s.testEvents)
	t.Run("FastExitInit", s.testFastExitInit)
}

// testLifecycle drives a container through create / start / state /
// kill / wait / delete / shutdown and verifies output appears.
func (s *RunSuite) testLifecycle(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	t.Log("shim binary:", shimBin)

	createOCISpec(t, bundleDir, []string{"/bin/forever", "hello"}, s.cfg)

	containerID := containerID(t)
	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "run")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	t.Log("shim started, address:", params.Address)

	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	drainFifoInto(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	drainFifo(t, ctx, stderrPath)

	t.Log("creating task")
	createResp, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, stdoutPath, stderrPath, rootfsMounts))
	if err != nil {
		t.Fatal("failed to create task:", err)
	}
	t.Log("task created, pid:", createResp.Pid)

	t.Log("starting task")
	startResp, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID})
	if err != nil {
		t.Fatal("failed to start task:", err)
	}
	t.Log("task started, pid:", startResp.Pid)

	t.Log("checking state")
	stateResp, err := tc.State(ctx, &taskAPI.StateRequest{ID: containerID})
	if err != nil {
		t.Fatal("failed to get state:", err)
	}
	t.Log("task state:", stateResp.Status)
	if stateResp.Status != tasktypes.Status_RUNNING {
		t.Fatalf("expected task status RUNNING, got %s", stateResp.Status)
	}

	t.Log("waiting for output")
	deadline := time.After(30 * time.Second)
	for {
		stdoutMu.Lock()
		got := stdoutBuf.String()
		stdoutMu.Unlock()
		if strings.Contains(got, "hello") {
			t.Log("got expected output:", strings.TrimSpace(got))
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for 'hello' output, got: %q", got)
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.Log("killing task")
	if _, err := tc.Kill(ctx, &taskAPI.KillRequest{
		ID:     containerID,
		Signal: uint32(syscall.SIGKILL),
		All:    true,
	}); err != nil {
		t.Fatal("failed to kill task:", err)
	}

	t.Log("waiting for task exit")
	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	if err != nil {
		t.Fatal("failed to wait for task:", err)
	}
	t.Log("task exited, status:", waitResp.ExitStatus)

	t.Log("deleting task")
	deleteResp, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	if err != nil {
		t.Fatal("failed to delete task:", err)
	}
	t.Log("task deleted, pid:", deleteResp.Pid, "exit status:", deleteResp.ExitStatus)

	t.Log("shutting down shim")
	shutdownTask(ctx, tc, containerID)
}

// testInitExitCodes runs /bin/exit N as the container's init for a
// range of values and verifies the task-level exit status.
func (s *RunSuite) testInitExitCodes(t *testing.T) {
	for _, code := range []int{0, 1, 2, 42, 127, 255} {
		t.Run(fmt.Sprintf("Exit%d", code), func(t *testing.T) {
			shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
			cid := containerID(t)

			createOCISpec(t, bundleDir, []string{"/bin/exit", strconv.Itoa(code)}, s.cfg)

			stdoutPath, stderrPath := createIOFifos(t, bundleDir)
			ns := uniqueTestNamespace(t, "run")
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

			waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
			if err != nil {
				t.Fatal("wait failed:", err)
			}
			if waitResp.ExitStatus != uint32(code) {
				t.Fatalf("expected exit status %d, got %d", code, waitResp.ExitStatus)
			}

			tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
			shutdownTask(ctx, tc, cid)
		})
	}
}

// testOutputThenExit runs a process that writes 50 lines then exits
// non-zero, and verifies both the exit status and every output line
// are captured by the shim.
func (s *RunSuite) testOutputThenExit(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/tickexit"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "run")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	drainFifoInto(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID}); err != nil {
		t.Fatal("failed to start task:", err)
	}

	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	if err != nil {
		t.Fatal("wait failed:", err)
	}
	t.Log("task exit status:", waitResp.ExitStatus)

	const expectedExit = 7
	if waitResp.ExitStatus != expectedExit {
		t.Fatalf("expected exit status %d, got %d", expectedExit, waitResp.ExitStatus)
	}

	deadline := time.After(5 * time.Second)
	for {
		stdoutMu.Lock()
		got := stdoutBuf.String()
		stdoutMu.Unlock()
		if strings.Contains(got, "tick 50\n") {
			break
		}
		select {
		case <-deadline:
			stdoutMu.Lock()
			final := stdoutBuf.String()
			stdoutMu.Unlock()
			t.Fatalf("timed out waiting for final output, got: %q", final)
		case <-time.After(10 * time.Millisecond):
		}
	}

	stdoutMu.Lock()
	got := stdoutBuf.String()
	stdoutMu.Unlock()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("expected 50 output lines, got %d: %q", len(lines), got)
	}
	for i, line := range lines {
		want := fmt.Sprintf("tick %d", i+1)
		if line != want {
			t.Fatalf("line %d: got %q, want %q", i, line, want)
		}
	}

	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}

// testEvents verifies that the shim publishes the expected task
// lifecycle events (create, start, exit, delete) to its containerd
// events endpoint during a normal run.
func (s *RunSuite) testEvents(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)
	ns := uniqueTestNamespace(t, "run")

	createOCISpec(t, bundleDir, []string{"/bin/exit", "0"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ctx := namespaces.WithNamespace(t.Context(), ns)

	rec := startEventsRecorder(t, bundleDir)

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

	if _, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID}); err != nil {
		t.Fatal("wait failed:", err)
	}

	if rec.waitForTopic("/tasks/exit", 2*time.Second) == nil {
		t.Fatalf("exit event not published after task wait; received topics: %v", rec.topics())
	}

	if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID}); err != nil {
		t.Fatal("delete failed:", err)
	}

	want := []string{
		"/tasks/create",
		"/tasks/start",
		"/tasks/exit",
		"/tasks/delete",
	}
	for _, topic := range want {
		if env := rec.waitForTopic(topic, 5*time.Second); env == nil {
			t.Fatalf("missing event %q; received topics: %v", topic, rec.topics())
		}
	}

	shutdownTask(ctx, tc, containerID)

	got := rec.topics()
	t.Log("received topics:", got)
	idx := 0
	for _, topic := range got {
		if idx < len(want) && topic == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Fatalf("events out of order or missing: want (in order) %v, got %v", want, got)
	}

	exitEnv := rec.waitForTopic("/tasks/exit", 0)
	if exitEnv == nil {
		t.Fatal("exit envelope vanished")
	}
	if exitEnv.Namespace != ns {
		t.Errorf("exit envelope namespace: got %q, want %q", exitEnv.Namespace, ns)
	}
	var exitEvt events.TaskExit
	if err := typeurl.UnmarshalTo(exitEnv.Event, &exitEvt); err != nil {
		t.Fatal("unmarshal TaskExit:", err)
	}
	if exitEvt.ContainerID != containerID {
		t.Errorf("exit event container id: got %q, want %q", exitEvt.ContainerID, containerID)
	}
	if exitEvt.ExitStatus != 0 {
		t.Errorf("exit event exit status: got %d, want 0", exitEvt.ExitStatus)
	}
}

// testFastExitInit runs /bin/burstexit as the container's init process
// (writes 8 MiB then exits immediately), then tears down the shim via
// Shutdown without calling Delete first. This forces ioShutdown to run
// from service.shutdown → container.shutdown rather than the orderly
// in-VM delete+drain sequence.
//
// The race: service.shutdown → c.shutdown → ioShutdown closes the
// vsock connection while the host copy goroutine may still be blocked
// in io.CopyBuffer's Read, draining bytes sitting in the kernel socket
// receive buffer. A buggy shim closes the connection first, causing
// the goroutine to see "use of closed network connection" and abandon
// the remaining buffered bytes.
//
// A correct shim waits for ioDone (copy goroutines finished) before
// closing the connections, so the full 8 MiB arrives intact.
func (s *RunSuite) testFastExitInit(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/burstexit", strconv.Itoa(burstPayloadSize), "0"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "run")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, containerID, ns, s.cfg)
	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	// drainFifoIntoDone closes done when the shim closes the FIFO write
	// end, which happens only after ioShutdown completes (i.e. after
	// the copy goroutine calls wc.Close). We must not read stdoutBuf
	// before that signal.
	drainDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, containerID, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: containerID}); err != nil {
		t.Fatal("start failed:", err)
	}

	// Shut down via Shutdown immediately after Start — without waiting
	// for the init to exit and without calling Delete. This triggers
	// service.shutdown → c.shutdown → ioShutdown while burstexit is
	// still running (writing its 8 MiB burst) or has just exited with
	// bytes still buffered in the vsock receive queue. The in-VM
	// delete+drain sequence (waitTimeout on the IO copy wg) never runs.
	tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: containerID})

	// Wait for the FIFO reader to reach EOF before inspecting the buffer.
	select {
	case <-drainDone:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for stdout drain after shutdown")
	}

	stdoutMu.Lock()
	got := stdoutBuf.Bytes()
	stdoutMu.Unlock()

	if len(got) != burstPayloadSize {
		t.Fatalf("got %d bytes, want %d (truncated by %d bytes)",
			len(got), burstPayloadSize, burstPayloadSize-len(got))
	}
	wantCRC := burstExpectedCRC32()
	gotCRC := crc32.Checksum(got, crc32.MakeTable(crc32.Castagnoli))
	if gotCRC != wantCRC {
		t.Fatalf("CRC mismatch: got %08x, want %08x (data corrupted)", gotCRC, wantCRC)
	}
	t.Logf("ok — %d bytes, CRC %08x", len(got), gotCRC)
}
