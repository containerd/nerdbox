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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Bench runs every benchmark in the ExecSuite as a sub-benchmark of
// b.
func (s *ExecSuite) Bench(b *testing.B) {
	b.Helper()
	b.Run("Exec", s.benchExec)
	b.Run("StdioRoundTrip", s.benchStdioRoundTrip)
	b.Run("ReadLargeFile", s.benchReadLargeFile)
	b.Run("ReadBindMount", s.benchReadBindMount)
}

// benchExec measures the time to exec a process inside a running
// container (exec + start + wait + delete). The container is set up
// once outside the benchmark loop.
func (s *ExecSuite) benchExec(b *testing.B) {
	shimBin, bundleDir, rootfsMounts := shimSetup(b, s.cfg)
	cid := containerID(b)
	ns := shimtestNamespace

	createOCISpec(b, bundleDir, []string{"/bin/forever"}, s.cfg)
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

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/echo", "benchexec"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		b.Fatal("marshal exec spec:", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		execID := fmt.Sprintf("exec-%d", i)
		execStdout, execStderr := createIOFifos(b, b.TempDir())
		drainFifo(b, ctx, execStdout)
		drainFifo(b, ctx, execStderr)
		b.StartTimer()

		if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
			ID:     cid,
			ExecID: execID,
			Spec:   procSpec,
			Stdout: execStdout,
			Stderr: execStderr,
		}); err != nil {
			b.Fatal("exec failed:", err)
		}
		if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid, ExecID: execID}); err != nil {
			b.Fatal("exec start failed:", err)
		}
		if _, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid, ExecID: execID}); err != nil {
			b.Fatal("exec wait failed:", err)
		}
		if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid, ExecID: execID}); err != nil {
			b.Fatal("exec delete failed:", err)
		}
	}
	b.StopTimer()

	tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
}

// benchStdioRoundTrip measures the time for a write-to-stdin /
// read-from-stdout round trip through a container running `cat`.
// Only the write+read is timed. Sub-benchmarks cover different
// payload sizes.
func (s *ExecSuite) benchStdioRoundTrip(b *testing.B) {
	shimBin, bundleDir, rootfsMounts := shimSetup(b, s.cfg)
	cid := containerID(b)
	ns := shimtestNamespace

	createOCISpec(b, bundleDir, []string{"/bin/forever"}, s.cfg)
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

	execID := "stdio-rt"
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
		Args: []string{"/bin/cat"},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
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

	// Warmup.
	warmup := make([]byte, 8)
	rand.Read(warmup)
	if _, err := stdinFifo.Write(warmup); err != nil {
		b.Fatal("warmup write:", err)
	}
	warmupBuf := make([]byte, len(warmup))
	if _, err := io.ReadFull(stdoutFifo, warmupBuf); err != nil {
		b.Fatal("warmup read:", err)
	}

	sizes := []struct {
		name string
		size int
	}{
		{"8B", 8},
		{"4KB", 4096},
		{"4MB", 4 * 1024 * 1024},
	}

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
				if _, err := io.ReadFull(stdoutFifo, recvBuf); err != nil {
					b.Fatal("read:", err)
				}
				if err := <-errCh; err != nil {
					b.Fatal("write:", err)
				}
			}
			b.StopTimer()
		})
	}

	stdinFifo.Close()
	tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid, ExecID: execID})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
}

// benchReadLargeFile measures the time to read + crc32 a 64 MiB
// fixture from the secondary erofs layer.
func (s *ExecSuite) benchReadLargeFile(b *testing.B) {
	s.benchHashverify(b, bigFileContainerPath, bigFileHashHex(), nil)
}

// benchReadBindMount is the bind-mount counterpart of
// benchReadLargeFile.
func (s *ExecSuite) benchReadBindMount(b *testing.B) {
	hostFile := filepath.Join(b.TempDir(), "bigfile")
	f, err := os.Create(hostFile)
	if err != nil {
		b.Fatal("create host bigfile:", err)
	}
	if _, err := io.Copy(f, newBigFileReader()); err != nil {
		f.Close()
		b.Fatal("write host bigfile:", err)
	}
	if err := f.Close(); err != nil {
		b.Fatal("close host bigfile:", err)
	}

	const containerPath = "/tmp/bigfile"
	s.benchHashverify(b, containerPath, bigFileHashHex(), []specs.Mount{{
		Type:        "bind",
		Source:      hostFile,
		Destination: containerPath,
		Options:     []string{"rbind"},
	}})
}

// benchHashverify is the shared driver for the two read benchmarks.
// Only the exec+start+wait of hashverify is timed; the per-iteration
// FIFO and exec-delete work is excluded via StopTimer/StartTimer.
func (s *ExecSuite) benchHashverify(b *testing.B, path, hashHex string, extraMounts []specs.Mount) {
	shimBin, bundleDir, rootfsMounts := shimSetup(b, s.cfg)
	cid := containerID(b)
	ns := shimtestNamespace

	var opts []func(*specs.Spec)
	if len(extraMounts) > 0 {
		opts = append(opts, withExtraMounts(extraMounts...))
	}
	createOCISpec(b, bundleDir, []string{"/bin/forever"}, s.cfg, opts...)

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

	procSpec, err := typeurl.MarshalAnyToProto(&specs.Process{
		Args: []string{"/bin/hashverify", path, hashHex},
		Cwd:  "/",
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		b.Fatal("marshal exec spec:", err)
	}

	b.SetBytes(bigFileSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		execID := fmt.Sprintf("hashv-%d", i)
		execStdout, execStderr := createIOFifos(b, b.TempDir())
		drainFifo(b, ctx, execStdout)
		drainFifo(b, ctx, execStderr)
		b.StartTimer()

		if _, err := tc.Exec(ctx, &taskAPI.ExecProcessRequest{
			ID:     cid,
			ExecID: execID,
			Spec:   procSpec,
			Stdout: execStdout,
			Stderr: execStderr,
		}); err != nil {
			b.Fatal("exec failed:", err)
		}
		if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid, ExecID: execID}); err != nil {
			b.Fatal("exec start failed:", err)
		}
		waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid, ExecID: execID})
		if err != nil {
			b.Fatal("exec wait failed:", err)
		}
		if waitResp.ExitStatus != 0 {
			b.Fatalf("hashverify exit %d", waitResp.ExitStatus)
		}

		b.StopTimer()
		if _, err := tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid, ExecID: execID}); err != nil {
			b.Fatal("exec delete failed:", err)
		}
		b.StartTimer()
	}

	b.StopTimer()
	tc.Kill(ctx, &taskAPI.KillRequest{ID: cid, Signal: uint32(syscall.SIGKILL), All: true})
	tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	tc.Shutdown(ctx, &taskAPI.ShutdownRequest{ID: cid})
}
