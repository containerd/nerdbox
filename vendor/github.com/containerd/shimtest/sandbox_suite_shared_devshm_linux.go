//go:build linux

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
	"strings"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// devShmMount is the /dev/shm mount exactly as sent by a real CRI client
// for a pod's member containers (confirmed against a live cluster via
// `crictl inspect`): every member container gets this same, independent
// tmpfs mount in its own spec — there is no shared-mount indication in the
// request at all. Deliberately not shimtest's default: the default OCI
// spec createOCISpec builds has no /dev/shm entry, so tests that don't care
// about it aren't affected, and this test is explicit about the exact
// shape it depends on.
var devShmMount = specs.Mount{
	Destination: "/dev/shm",
	Type:        "tmpfs",
	Source:      "shm",
	Options:     []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
}

// testMemberContainersShareDevShm verifies that member containers of the
// same sandbox that share an IPC namespace also get a working, shared
// /dev/shm: a POSIX-shared-memory-style write — through an
// mmap(MAP_SHARED) mapping, not a plain write(2) — made by one member
// container is visible, through an independent mmap(MAP_SHARED) mapping,
// to a second, independently created member container.
//
// The API contract: Kubernetes pods share both an IPC namespace and
// /dev/shm across their containers by default (the same
// shareProcessNamespace / hostIPC-independent default this suite's IPC
// namespace test exercises). A container's own OCI spec carries no
// separate signal for "share /dev/shm too" — CRI clients send every
// member container the same, independent-looking
// {Type: "tmpfs", Destination: "/dev/shm"} mount — so the shim must infer
// /dev/shm sharing from the same "this container is joining a shared IPC
// namespace" signal used for IPC namespace sharing itself, and must not
// rely on the container's own /dev/shm mount already looking shared.
//
// The write and read paths deliberately go through mmap rather than
// read(2)/write(2): what's under test is whether two independent
// mmap(MAP_SHARED) mappings of what should be the same underlying file —
// made by processes in different containers, each reaching the file
// through whatever mechanism the shim uses to make it appear at
// /dev/shm — actually share memory, which is a stronger and more direct
// test of POSIX shared-memory semantics than confirming that file
// contents eventually match.
func (s *SandboxSuite) testMemberContainersShareDevShm(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	const (
		shmPath = "/dev/shm/testfile"
		marker  = "shared-devshm-ok"
	)

	writerCID := createContainerInSandbox(t, env, []string{"/bin/shmmapwrite", shmPath, marker},
		withSandboxCtrNamespace(specs.IPCNamespace, "/proc/1/ns/ipc"),
		withSandboxCtrExtraMounts(devShmMount))

	writerWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: writerCID})
	if err != nil {
		t.Fatalf("Task.Wait writer: %v", err)
	}
	if writerWait.GetExitStatus() != 0 {
		t.Fatalf("writer container exit status: got %d, want 0", writerWait.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: writerCID}) //nolint:errcheck

	readerCID := createContainerInSandbox(t, env, []string{"/bin/shmmapread", shmPath},
		withSandboxCtrNamespace(specs.IPCNamespace, "/proc/1/ns/ipc"),
		withSandboxCtrExtraMounts(devShmMount))

	out := readContainerOutput(t, env, readerCID, marker, 30*time.Second)
	if !strings.Contains(out, marker) {
		t.Fatalf("shmmapread output did not contain marker %q: %q", marker, out)
	}

	readerWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: readerCID})
	if err != nil {
		t.Fatalf("Task.Wait reader: %v", err)
	}
	if readerWait.GetExitStatus() != 0 {
		t.Fatalf("reader container exit status: got %d, want 0", readerWait.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: readerCID}) //nolint:errcheck

	t.Log("member containers share /dev/shm: mmap(MAP_SHARED) write visible across containers")
}

// testMemberContainersDevShmNotSharedWithoutIPC verifies the negative case:
// a member container that does *not* share an IPC namespace (the default —
// NamespaceMode_CONTAINER) gets its own private /dev/shm, even though its
// spec carries the exact same-looking {Type: "tmpfs", Destination:
// "/dev/shm"} mount as a sharing container's does. This guards against a
// shim inferring sharing from the mount's shape (which is identical either
// way) rather than from the IPC-sharing signal.
func (s *SandboxSuite) testMemberContainersDevShmNotSharedWithoutIPC(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	const (
		shmPath = "/dev/shm/testfile"
		marker  = "should-not-be-visible"
	)

	writerCID := createContainerInSandbox(t, env, []string{"/bin/shmmapwrite", shmPath, marker},
		withSandboxCtrExtraMounts(devShmMount))
	// No withSandboxCtrNamespace(IPCNamespace, ...): this container does not
	// request IPC sharing.

	writerWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: writerCID})
	if err != nil {
		t.Fatalf("Task.Wait writer: %v", err)
	}
	if writerWait.GetExitStatus() != 0 {
		t.Fatalf("writer container exit status: got %d, want 0", writerWait.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: writerCID}) //nolint:errcheck

	readerCID := createContainerInSandbox(t, env, []string{"/bin/shmmapread", shmPath},
		withSandboxCtrExtraMounts(devShmMount))

	// Wait for the reader's actual, specific "not found" output (see
	// cmdShmMapRead) rather than an empty want string: strings.Contains
	// treats "" as a match against anything, including a buffer that
	// hasn't received any output yet, which would let this pass without
	// having waited for the container to actually run at all.
	out := readContainerOutput(t, env, readerCID, "NOTFOUND", 30*time.Second)
	if strings.Contains(out, marker) {
		t.Fatalf("reader saw writer's marker %q despite neither container sharing IPC: %q", marker, out)
	}

	readerWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: readerCID})
	if err != nil {
		t.Fatalf("Task.Wait reader: %v", err)
	}
	if readerWait.GetExitStatus() == 0 {
		t.Fatalf("reader container exit status: got 0, want non-zero (file should not exist in its private /dev/shm)")
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: readerCID}) //nolint:errcheck

	t.Log("member containers not sharing IPC correctly get independent, private /dev/shm instances")
}
