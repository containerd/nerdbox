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
	"syscall"
	"testing"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
)

// OOMSuite contains the OOM-killer test, gated on the "oom" feature
// (some shim configurations don't expose memory cgroup controls or
// can't reliably trigger the kernel OOM killer).
type OOMSuite struct {
	cfg Config
	
}

// NewOOMSuite constructs an OOMSuite from the given options.
func NewOOMSuite(cfg Config) *OOMSuite {
	return &OOMSuite{cfg: cfg}
}

// Run runs every test in the suite as a subtest of t.
func (s *OOMSuite) Run(t *testing.T) {
	t.Helper()
	registerShimLeakCheck(t, s.cfg.ShimBinary)
	t.Run("OOM", s.testOOM)
}

// TestOOM runs a memory-hungry process inside a container with a
// 128MiB memory limit and verifies the kernel OOM-kills it (exit 137).
func (s *OOMSuite) testOOM(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	createOCISpec(t, bundleDir, []string{"/bin/memhog"}, s.cfg,
		withMemoryLimit(128*1024*1024),
	)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "oom")
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

	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	if err != nil {
		t.Fatal("wait failed:", err)
	}
	t.Log("task exit status:", waitResp.ExitStatus)

	const sigkillExit = 128 + uint32(syscall.SIGKILL)
	if waitResp.ExitStatus != sigkillExit {
		t.Fatalf("expected exit status %d (SIGKILL from OOM killer), got %d",
			sigkillExit, waitResp.ExitStatus)
	}

	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}
