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
	"net"
	"os"
	"path/filepath"
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

// UDSSuite contains the UDS-mount tests, gated on the "uds" feature.
type UDSSuite struct {
	cfg Config
	
}

// NewUDSSuite constructs a UDSSuite from the given options.
func NewUDSSuite(cfg Config) *UDSSuite {
	return &UDSSuite{cfg: cfg}
}

// Run runs every test in the suite as a subtest of t. The subtest
// name is kept as UDSRoundTrip to match historical -test.run filters.
func (s *UDSSuite) Run(t *testing.T) {
	t.Helper()
	registerShimLeakCheck(t, s.cfg.ShimBinary)
	t.Run("UDSRoundTrip", s.testRoundTrip)
}

// TestRoundTrip verifies that a container can connect to a host-side
// unix domain socket via the UDS mount type, and that data flows
// bidirectionally.
func (s *UDSSuite) testRoundTrip(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	containerID := containerID(t)

	hostSockDir, err := os.MkdirTemp(unixSafeDir(), "nb-uds-")
	if err != nil {
		t.Fatal("create uds dir:", err)
	}
	t.Cleanup(func() { os.RemoveAll(hostSockDir) })
	hostSockPath := filepath.Join(hostSockDir, "test.sock")
	ln, err := net.Listen("unix", hostSockPath)
	if err != nil {
		t.Fatal("listen:", err)
	}
	defer ln.Close()

	const containerSockPath = "/run/test.sock"

	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg,
		withExtraMounts(specs.Mount{
			Type:        "uds",
			Source:      hostSockPath,
			Destination: containerSockPath,
		}),
	)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "uds")
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

	execID := "uds-rt"
	execDir := t.TempDir()
	_, execStdout, execStderr := createStdioFifos(t, execDir)

	var execBuf bytes.Buffer
	var execMu sync.Mutex
	drainFifoInto(t, ctx, execStdout, &execBuf, &execMu)
	drainFifo(t, ctx, execStderr)

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/nc", "-U", containerSockPath},
		Cwd:  "/",
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

	ln.(*net.UnixListener).SetDeadline(time.Now().Add(30 * time.Second))
	hostConn, err := ln.Accept()
	if err != nil {
		t.Fatal("accept:", err)
	}
	defer hostConn.Close()

	token := randomSuffix() + "\n"
	if _, err := hostConn.Write([]byte(token)); err != nil {
		t.Fatal("host write:", err)
	}

	deadline := time.After(30 * time.Second)
	for {
		execMu.Lock()
		got := execBuf.String()
		execMu.Unlock()
		if bytes.Contains([]byte(got), []byte(token)) {
			t.Log("UDS round trip succeeded, got:", got)
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for UDS round trip, got: %q", got)
		case <-time.After(10 * time.Millisecond):
		}
	}

	hostConn.Close()
	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, ExecID: execID, Signal: uint32(syscall.SIGKILL)})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID, ExecID: execID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID, ExecID: execID})
	tc.Kill(ctx, &taskAPI.KillRequest{ID: containerID, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: containerID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: containerID})
	shutdownTask(ctx, tc, containerID)
}
