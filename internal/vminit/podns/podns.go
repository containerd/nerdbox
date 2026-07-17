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

// Package podns creates, on demand, the persistent guest-side IPC and PID
// namespaces that member containers of a sandbox join when the pod's CRI
// NamespaceOptions request POD-level sharing (see internal/podns for the
// shared path constants and the rationale, and internal/vminit/podpause
// for the PID namespace's anchor process).
package podns

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/containerd/nerdbox/internal/podns"
)

// Manager creates the sandbox's shared IPC and PID namespaces the first
// time they're requested, and returns their guest paths on every
// subsequent call without doing any work again. A single Manager is
// meant to be shared for the lifetime of one vminitd process (one
// sandbox).
type Manager struct {
	mu    sync.Mutex
	ready bool
	err   error // sticky: a failed first attempt is not silently retried
}

// EnsureNamespaces creates the shared IPC and PID namespaces if they do
// not already exist, and returns their guest paths. Safe to call
// concurrently and repeatedly; only the first call does any work.
func (m *Manager) EnsureNamespaces(ctx context.Context) (ipcPath, pidPath string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ready {
		return podns.IPCPath, podns.PIDPath, nil
	}
	if m.err != nil {
		return "", "", m.err
	}

	if err := createIPCNamespace(podns.IPCPath); err != nil {
		m.err = fmt.Errorf("create shared ipc namespace: %w", err)
		return "", "", m.err
	}
	if err := createPIDAnchor(ctx, podns.PIDPath); err != nil {
		m.err = fmt.Errorf("create shared pid namespace: %w", err)
		return "", "", m.err
	}

	m.ready = true
	return podns.IPCPath, podns.PIDPath, nil
}

// createIPCNamespace creates a new IPC namespace and bind-mounts it to
// path, using the same "persistent namespace" technique
// internal/vminit/podnetns uses for the network namespace: a dedicated
// goroutine locks itself to an OS thread, unshares a new IPC namespace on
// that thread (which, unlike CLONE_NEWPID, takes effect on the calling
// thread immediately), and bind-mounts it. The bind-mount is what keeps
// the namespace alive; the creating goroutine does not need to stay
// alive afterward.
func createIPCNamespace(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return fmt.Errorf("create bind-mount target: %w", err)
	}
	f.Close()

	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Intentionally no UnlockOSThread: see the identical pattern (and
		// rationale) in internal/vminit/podnetns.Create.

		if err := unix.Unshare(unix.CLONE_NEWIPC); err != nil {
			errCh <- fmt.Errorf("unshare CLONE_NEWIPC: %w", err)
			return
		}
		nsSrc := fmt.Sprintf("/proc/self/task/%d/ns/ipc", unix.Gettid())
		if err := unix.Mount(nsSrc, path, "", unix.MS_BIND, ""); err != nil {
			errCh <- fmt.Errorf("bind mount %s -> %s: %w", nsSrc, path, err)
			return
		}
		errCh <- nil
	}()
	return <-errCh
}

// createPIDAnchor starts the pod-pause anchor process (see
// internal/vminit/podpause) in a new PID namespace and bind-mounts that
// namespace to path.
//
// Unlike CLONE_NEWIPC/CLONE_NEWNET/CLONE_NEWUTS, unshare(CLONE_NEWPID)
// does not move the calling thread into the new namespace — it only
// causes the *next process the caller forks* to become PID 1 of a new
// namespace. A goroutine or OS thread can never itself be PID 1: PID 1
// must be a real, distinct process, and if it ever exits, the kernel
// tears down the entire namespace (and kills everything in it). So this
// creates the namespace by starting a real child process with
// SysProcAttr.Cloneflags: CLONE_NEWPID, rather than by unsharing on a
// locked thread the way the IPC namespace above does.
func createPIDAnchor(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return fmt.Errorf("create bind-mount target: %w", err)
	}
	f.Close()

	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("resolve /proc/self/exe: %w", err)
	}

	cmd := exec.Command(exe, "pod-pause")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start pod-pause anchor: %w", err)
	}

	nsSrc := fmt.Sprintf("/proc/%d/ns/pid", cmd.Process.Pid)
	if err := unix.Mount(nsSrc, path, "", unix.MS_BIND, ""); err != nil {
		// The bind mount is what's supposed to keep the anchor's
		// namespace referenced (see the doc comment above); if it never
		// happens, nothing will ever wait on or kill this process, so it
		// would otherwise run for the rest of the VM's lifetime. Kill it
		// and wait synchronously here rather than leaking it.
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.G(ctx).WithError(killErr).Warn("failed to kill pod-pause anchor after mount failure")
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			log.G(ctx).WithError(waitErr).Warn("pod-pause anchor wait after mount failure")
		}
		return fmt.Errorf("bind mount %s -> %s: %w", nsSrc, path, err)
	}

	// Reap the anchor's own exit in the background (it should never exit
	// on its own — only via SIGKILL at sandbox teardown) so it never
	// becomes a zombie under vminitd. Started only once the bind mount
	// has succeeded: a mount failure above is handled synchronously so
	// this goroutine is never left running with nothing to wake it.
	go func() {
		if err := cmd.Wait(); err != nil {
			log.G(ctx).WithError(err).Warn("pod-pause anchor process exited")
		}
	}()

	return nil
}
