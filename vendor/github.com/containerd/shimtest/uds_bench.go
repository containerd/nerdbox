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
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Bench runs every benchmark in the UDSSuite as a sub-benchmark of
// b.
func (s *UDSSuite) Bench(b *testing.B) {
	b.Helper()
	b.Run("UDSRoundTrip", s.benchUDSRoundTrip)
}

// benchUDSRoundTrip measures one-way throughput across a
// UDS-forwarded socket in both directions at different payload
// sizes. The container runs `nc -U` which pipes socket↔stdio,
// giving us two independent data paths to time.
func (s *UDSSuite) benchUDSRoundTrip(b *testing.B) {
	shimBin, bundleDir, rootfsMounts := shimSetup(b, s.cfg)
	cid := containerID(b)
	ns := shimtestNamespace

	hostSockDir, err := os.MkdirTemp("/tmp", "nb-uds-")
	if err != nil {
		b.Fatal("create uds dir:", err)
	}
	b.Cleanup(func() { os.RemoveAll(hostSockDir) })
	hostSockPath := filepath.Join(hostSockDir, "bench.sock")
	ln, err := net.Listen("unix", hostSockPath)
	if err != nil {
		b.Fatal("listen:", err)
	}
	defer ln.Close()

	const containerSockPath = "/run/bench.sock"

	createOCISpec(b, bundleDir, []string{"/bin/forever"}, s.cfg,
		withExtraMounts(specs.Mount{
			Type:        "uds",
			Source:      hostSockPath,
			Destination: containerSockPath,
		}),
	)

	stdoutPath, stderrPath := createIOFifos(b, bundleDir)
	ctx := namespaces.WithNamespace(b.Context(), ns)

	params := startShim(b, shimBin, bundleDir, cid, ns, s.cfg)
	conn := connectShim(b, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	drainFifo(b, ctx, stdoutPath)
	drainFifo(b, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(b, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		b.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		b.Fatal("start failed:", err)
	}

	execID := "uds-bench"
	execDir := b.TempDir()
	execStdin, execStdout, execStderr := createStdioFifos(b, execDir)

	stdinFifo, err := openPipeWriter(ctx, execStdin)
	if err != nil {
		b.Fatal("open stdin pipe:", err)
	}
	defer stdinFifo.Close()

	stdoutFifo, err := openPipeReader(ctx, execStdout)
	if err != nil {
		b.Fatal("open stdout pipe:", err)
	}
	defer stdoutFifo.Close()

	drainFifo(b, ctx, execStderr)

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/nc", "-U", containerSockPath},
		Cwd:  "/",
	})
	if err != nil {
		b.Fatal("marshal exec spec:", err)
	}

	if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
		ID:     cid,
		ExecID: execID,
		Spec:   procSpec,
		Stdin:  execStdin,
		Stdout: execStdout,
		Stderr: execStderr,
	}); err != nil {
		b.Fatal("exec failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid, ExecID: execID}); err != nil {
		b.Fatal("exec start failed:", err)
	}

	ln.(*net.UnixListener).SetDeadline(time.Now().Add(30 * time.Second))
	hostConn, err := ln.Accept()
	if err != nil {
		b.Fatal("accept:", err)
	}
	defer hostConn.Close()

	sizes := []struct {
		name string
		size int
	}{
		{"8B", 8},
		{"4KB", 4096},
		{"4MB", 4 * 1024 * 1024},
	}

	b.Run("HostToContainer", func(b *testing.B) {
		for _, sz := range sizes {
			b.Run(sz.name, func(b *testing.B) {
				payload := make([]byte, sz.size)
				rand.Read(payload)
				recvBuf := make([]byte, sz.size)
				b.SetBytes(int64(sz.size))
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					errCh := make(chan error, 1)
					go func() {
						_, err := hostConn.Write(payload)
						errCh <- err
					}()
					if _, err := io.ReadFull(stdoutFifo, recvBuf); err != nil {
						b.Fatal("read stdout:", err)
					}
					if err := <-errCh; err != nil {
						b.Fatal("write hostConn:", err)
					}
				}
			})
		}
	})

	b.Run("ContainerToHost", func(b *testing.B) {
		for _, sz := range sizes {
			b.Run(sz.name, func(b *testing.B) {
				payload := make([]byte, sz.size)
				rand.Read(payload)
				recvBuf := make([]byte, sz.size)
				b.SetBytes(int64(sz.size))
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					errCh := make(chan error, 1)
					go func() {
						_, err := stdinFifo.Write(payload)
						errCh <- err
					}()
					if _, err := io.ReadFull(hostConn, recvBuf); err != nil {
						b.Fatal("read hostConn:", err)
					}
					if err := <-errCh; err != nil {
						b.Fatal("write stdin:", err)
					}
				}
			})
		}
	})

	stdinFifo.Close()
	hostConn.Close()
	tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid, ExecID: execID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
}
