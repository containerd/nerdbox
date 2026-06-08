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
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	transferapi "github.com/containerd/containerd/api/services/transfer/v1"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/dmcgowan/shimtest/internal/transfer"
)

// TransferSuite contains tests gated on the "transfer" feature: the
// transfer service's copy-in/out operations, the bidirectional-stream
// stress test, and a Fuzz target that callers can wire into a
// top-level FuzzXxx in their own _test.go.
type TransferSuite struct {
	cfg Config
}

// NewTransferSuite constructs a TransferSuite from the given options.
func NewTransferSuite(cfg Config) *TransferSuite {
	return &TransferSuite{cfg: cfg}
}

// Run runs every test in the suite as a subtest of t. Subtest names
// are kept stable (TransferCopyTo, TransferCopyToAndFrom,
// TransferExecVerify) so existing -test.run filters and CI workflow
// patterns keep matching. The transfer stress test moved to
// StressSuite; construct one of those with StressOptions{Transfer:
// true} to run it.
func (s *TransferSuite) Run(t *testing.T) {
	t.Helper()
	registerShimLeakCheck(t, s.cfg.ShimBinary)
	t.Run("TransferCopyTo", s.testCopyTo)
	t.Run("TransferCopyToAndFrom", s.testCopyToAndFrom)
	t.Run("TransferExecVerify", s.testExecVerify)
}

// TestCopyTo copies a small file into a running container via the
// transfer service.
func (s *TransferSuite) testCopyTo(t *testing.T) {
	env := newShimEnv(t, t.Context(), s.cfg, "transfer")
	skipIfNoTransfer(t, env)
	const testContent = "transfer-test-data-12345\n"

	copyToContainer(t, env.ctx, env, testContent, "/tmp")
	t.Log("copy-to succeeded")

	shutdownShim(t, env.ctx, env)
}

// TestCopyToAndFrom copies a file in then back out and verifies the
// content matches.
func (s *TransferSuite) testCopyToAndFrom(t *testing.T) {
	env := newShimEnv(t, t.Context(), s.cfg, "transfer")
	skipIfNoTransfer(t, env)
	const testContent = "transfer-test-data-12345\n"

	copyToContainer(t, env.ctx, env, testContent, "/tmp")
	t.Log("copy-to succeeded")

	received := copyFromContainer(t, env.ctx, env, "/tmp/transferred.txt")
	if received != testContent {
		t.Fatalf("copy-from content mismatch: got %q, want %q", received, testContent)
	}
	t.Log("copy-from verified, content:", strings.TrimSpace(received))

	shutdownShim(t, env.ctx, env)
}

// TestExecVerify copies a file in and verifies it via /bin/cat exec.
func (s *TransferSuite) testExecVerify(t *testing.T) {
	env := newShimEnv(t, t.Context(), s.cfg, "transfer")
	skipIfNoTransfer(t, env)
	const testContent = "transfer-test-data-12345\n"

	copyToContainer(t, env.ctx, env, testContent, "/tmp")

	execOutput := shimExec(t, env.ctx, env, "verify", []string{"/bin/cat", "/tmp/transferred.txt"})
	if !strings.Contains(execOutput, strings.TrimRight(testContent, "\n")) {
		t.Fatalf("exec output %q does not contain expected content %q", execOutput, testContent)
	}
	t.Log("copy-to verified via exec:", strings.TrimSpace(execOutput))

	shutdownShim(t, env.ctx, env)
}

// copyToContainer transfers content as a tar archive into the
// container at the given path using the transfer service.
func copyToContainer(t *testing.T, ctx context.Context, env *shimEnv, content, path string) {
	t.Helper()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "transferred.txt",
		Mode: 0644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal("failed to write tar header:", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal("failed to write tar data:", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal("failed to close tar writer:", err)
	}

	src := transfer.NewReadStream(&tarBuf, "application/x-tar")
	dst := &transfer.ContainerPath{
		ContainerID: env.containerID,
		Path:        path,
	}

	srcAny, err := marshalTransferAny(ctx, src, env.sc)
	if err != nil {
		t.Fatal("failed to marshal source:", err)
	}
	dstAny, err := marshalTransferAny(ctx, dst, env.sc)
	if err != nil {
		t.Fatal("failed to marshal destination:", err)
	}

	tfClient := transferapi.NewTTRPCTransferClient(env.client)
	if _, err := tfClient.Transfer(ctx, &transferapi.TransferRequest{
		Source:      srcAny,
		Destination: dstAny,
	}); err != nil {
		t.Fatal("copy-to transfer failed:", err)
	}
	// Wait for the background send goroutine to finish draining the
	// reader and surface any transport error it encountered.
	if err := src.Wait(ctx); err != nil {
		t.Fatal("copy-to stream error:", err)
	}
}

// copyFromContainer reads a file from the container via the transfer
// service and returns its content.
func copyFromContainer(t *testing.T, ctx context.Context, env *shimEnv, path string) string {
	t.Helper()

	src := &transfer.ContainerPath{
		ContainerID: env.containerID,
		Path:        path,
	}

	var received bytes.Buffer
	dst := transfer.NewWriteStream(&received, "application/x-tar")

	srcAny, err := marshalTransferAny(ctx, src, env.sc)
	if err != nil {
		t.Fatal("failed to marshal source:", err)
	}
	dstAny, err := marshalTransferAny(ctx, dst, env.sc)
	if err != nil {
		t.Fatal("failed to marshal destination:", err)
	}

	tfClient := transferapi.NewTTRPCTransferClient(env.client)
	if _, err := tfClient.Transfer(ctx, &transferapi.TransferRequest{
		Source:      srcAny,
		Destination: dstAny,
	}); err != nil {
		t.Fatal("copy-from transfer failed:", err)
	}
	// Wait for the background receive goroutine to finish draining the
	// stream into received. The Transfer RPC returning only confirms the
	// server is done writing; the client-side io.Copy runs in a separate
	// goroutine. Waiting here also surfaces any transport error rather
	// than silently delivering an empty or truncated result.
	if err := dst.Wait(ctx); err != nil {
		t.Fatal("copy-from stream error:", err)
	}

	tr := tar.NewReader(&received)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("failed to read tar:", err)
		}
		if strings.HasSuffix(hdr.Name, "transferred.txt") {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal("failed to read tar entry:", err)
			}
			return string(data)
		}
	}

	t.Fatal("transferred.txt not found in tar archive")
	return ""
}

// shimExec runs a process inside the container via the Exec API and
// returns its stdout output.
func shimExec(t *testing.T, ctx context.Context, env *shimEnv, execID string, args []string) string {
	t.Helper()

	execStdout, execStderr := createIOFifos(t, t.TempDir())

	var execBuf bytes.Buffer
	var execMu sync.Mutex
	drainFifoInto(t, ctx, execStdout, &execBuf, &execMu)
	drainFifo(t, ctx, execStderr)

	procSpec := &specs.Process{
		Args: args,
		Cwd:  "/",
		Env:  []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}
	execSpec, err := typeurl.MarshalAnyToProto(procSpec)
	if err != nil {
		t.Fatal("failed to marshal exec spec:", err)
	}

	if _, err := env.tc.Exec(ctx, &taskAPI.ExecProcessRequest{
		ID:     env.containerID,
		ExecID: execID,
		Spec:   execSpec,
		Stdout: execStdout,
		Stderr: execStderr,
	}); err != nil {
		t.Fatal("exec failed:", err)
	}

	if _, err := env.tc.Start(ctx, &taskAPI.StartRequest{
		ID:     env.containerID,
		ExecID: execID,
	}); err != nil {
		t.Fatal("exec start failed:", err)
	}

	waitResp, err := env.tc.Wait(ctx, &taskAPI.WaitRequest{
		ID:     env.containerID,
		ExecID: execID,
	})
	if err != nil {
		t.Fatal("exec wait failed:", err)
	}
	if waitResp.ExitStatus != 0 {
		t.Fatalf("exec exited with status %d", waitResp.ExitStatus)
	}

	time.Sleep(100 * time.Millisecond)

	if _, err := env.tc.Delete(ctx, &taskAPI.DeleteRequest{
		ID:     env.containerID,
		ExecID: execID,
	}); err != nil {
		t.Fatal("exec delete failed:", err)
	}

	execMu.Lock()
	defer execMu.Unlock()
	return execBuf.String()
}

// Fuzz exposes the missing-file fuzz body for use by a top-level
// FuzzXxx in any package. The caller's FuzzTransferMissing should
// look like:
//
//	func FuzzTransferMissing(f *testing.F) {
//	    shimtest.NewTransferSuite(opts).Fuzz(f)
//	}
//
// Each fuzz iteration synthesizes a path under a never-created base
// directory and asserts the transfer service returns an
// application-level error (not a timeout).
func (s *TransferSuite) Fuzz(f *testing.F) {
	env := newShimEnv(f, f.Context(), s.cfg, "transfer")
	skipIfNoTransfer(f, env)
	defer shutdownShim(f, env.ctx, env)

	f.Add("missing.txt")
	f.Add("a/b/c/d.txt")
	f.Add("does-not-exist")
	f.Add(strings.Repeat("a", 200))
	f.Add(".hidden")

	f.Fuzz(func(t *testing.T, suffix string) {
		if !validFuzzSuffix(suffix) {
			t.Skip("rejected fuzz suffix")
		}
		path := fuzzMissingBase + "/" + suffix

		err := stressTransferStat(env.ctx, env, path)
		if err == nil {
			t.Errorf("expected error for missing path %q, got nil", path)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Errorf("path %q: server did not respond before iteration deadline: %v", path, err)
		}
	})
}

// validFuzzSuffix gates fuzz inputs to suffixes that, when joined to
// fuzzMissingBase, stay inside that directory and don't contain
// bytes the shim's path layer can't represent. Rejected inputs are
// skipped (not failed).
func validFuzzSuffix(s string) bool {
	if s == "" || len(s) > 4000 {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return false
	}
	if strings.Contains(s, "\x00") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}
