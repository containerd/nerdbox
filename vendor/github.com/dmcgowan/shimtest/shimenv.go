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
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	streamingapi "github.com/containerd/containerd/api/services/streaming/v1"
	transferapi "github.com/containerd/containerd/api/services/transfer/v1"
	"github.com/containerd/containerd/v2/core/streaming"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
)

// shimEnv is the per-test shim environment. It wraps the shim's
// TTRPC client, task service, streaming service, and the running
// container's identity. Suite tests call newShimEnv to construct
// one; cleanup is registered on the supplied testing.TB.
type shimEnv struct {
	ctx         context.Context
	client      *ttrpc.Client
	tc          taskAPI.TTRPCTaskService
	sc          streaming.StreamCreator
	containerID string
	// shimPID is the OS pid of the shim process, read from
	// bundleDir/shim.pid after startShim. Zero if the file was
	// missing or unparseable.
	shimPID int
}

// newShimEnv starts a shim, creates and starts a container, and
// returns a shimEnv ready for task/transfer operations. Cleanup is
// registered via tb.Cleanup. The container runs /bin/forever as its
// init process. Tests that need a different command build their own
// env via the lower-level helpers (shimSetup, createOCISpec,
// startShim, ...).
//
// suite is the short name of the calling suite (e.g. "stress",
// "transfer") and is forwarded to uniqueTestNamespace so that the
// containerd namespace used by this shim encodes the suite for
// zombie-process attribution.
func newShimEnv(tb testing.TB, baseCtx context.Context, cfg Config, suite string) *shimEnv {
	tb.Helper()

	shimBin, bundleDir, rootfsMounts := shimSetup(tb, cfg)

	cid := containerID(tb)

	createOCISpec(tb, bundleDir, []string{"/bin/forever"}, cfg)

	stdoutPath, stderrPath := createIOFifos(tb, bundleDir)

	ns := uniqueTestNamespace(tb, suite)
	ctx := namespaces.WithNamespace(baseCtx, ns)

	// Bind a no-op events listener so the shim's outgoing event
	// publishes succeed instead of timing out for 5s each.
	startEventsRecorder(tb, bundleDir)

	params := startShim(tb, shimBin, bundleDir, cid, ns, cfg)

	conn := connectShim(tb, params.Address)
	client := ttrpc.NewClient(conn)
	tb.Cleanup(func() { client.Close() })

	tc := taskAPI.NewTTRPCTaskClient(client)

	sc := &ttrpcStreamCreator{client: streamingapi.NewTTRPCStreamingClient(client)}

	drainFifo(tb, ctx, stdoutPath)
	drainFifo(tb, ctx, stderrPath)
	if _, err := tc.Create(ctx, newCreateTaskRequest(tb, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		tb.Fatal("failed to create task:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		tb.Fatal("failed to start task:", err)
	}

	// Ask the shim for its pid via the task service Connect RPC —
	// authoritative and cross-platform. Now that the task is created,
	// shims that gate Connect on task existence (e.g., nerdbox) will
	// also respond. Falls back to shim.pid for shims that fail
	// Connect entirely.
	shimPID := 0
	if pid, err := shimPidViaConnect(params.Address, cid, 1*time.Second); err == nil {
		shimPID = pid
	} else if data, err := os.ReadFile(filepath.Join(bundleDir, "shim.pid")); err == nil {
		shimPID, _ = parseIntBytes(data)
	}

	return &shimEnv{
		ctx:         ctx,
		client:      client,
		tc:          tc,
		sc:          sc,
		containerID: cid,
		shimPID:     shimPID,
	}
}

// skipIfNoTransfer probes the transfer service with an empty request
// and skips the test if the shim doesn't implement it. Tests that
// require the transfer service should call this immediately after
// newShimEnv.
func skipIfNoTransfer(tb testing.TB, env *shimEnv) {
	tb.Helper()
	tfClient := transferapi.NewTTRPCTransferClient(env.client)
	_, err := tfClient.Transfer(env.ctx, &transferapi.TransferRequest{})
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "Unimplemented") || strings.Contains(msg, "unknown service") {
		tb.Skip("skipping: shim does not support transfer service:", err)
	}
}

// shutdownShim kills, waits, deletes, and shuts down the shim's task.
// Best-effort: errors are logged on tb but not fatal, since it's
// commonly called as cleanup after a test that's already failed.
// The whole sequence is bounded by shutdownTimeout so a wedged shim
// can't hijack the test deadline.
func shutdownShim(tb testing.TB, ctx context.Context, env *shimEnv) {
	tb.Helper()

	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	tb.Log("killing task")
	env.tc.Kill(ctx, &taskAPI.KillRequest{
		ID:     env.containerID,
		Signal: uint32(syscall.SIGKILL),
		All:    true,
	})

	tb.Log("waiting for exit")
	env.tc.Wait(ctx, &taskAPI.WaitRequest{ID: env.containerID})

	tb.Log("deleting task")
	env.tc.Delete(ctx, &taskAPI.DeleteRequest{ID: env.containerID})

	tb.Log("shutting down shim")
	env.tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: env.containerID})
}

// shutdownTimeout caps the cumulative time shutdownShim spends across
// its Kill/Wait/Delete/Shutdown RPCs. Keeps a wedged shim from
// blocking test cleanup for the rest of the test deadline.
const shutdownTimeout = 10 * time.Second
