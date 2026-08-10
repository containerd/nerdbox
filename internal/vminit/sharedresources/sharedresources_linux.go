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

// Package sharedresources creates and deletes guest-side resources that
// containers sharing a sandbox use to share state with each other — Linux
// namespaces (IPC, PID, network) and other guest-side resources set up the
// same way (currently just a shared /dev/shm tmpfs) — and reports the guest
// paths at which they are pinned. It implements the SharedResources service
// declared in api/proto/nerdbox/services/sharedresources/v1; see that file
// for the contract and for why creation is per type rather than
// all-or-nothing.
package sharedresources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/containerd/log"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// anchorBinary is the anchor executable in the guest rootfs. It exists only
// to be PID 1 of a namespace: see createPID for why that needs a process, and
// crates/pause for the implementation.
const anchorBinary = "/sbin/nerdbox-pause"

// anchorCommand is the command createPID runs to anchor a PID namespace. It
// is a variable purely so tests can substitute a binary that exists on the
// host, since the real one only ships in the guest rootfs.
var anchorCommand = []string{anchorBinary}

// Type identifies a kind of guest-side shared resource this package can
// manage. It mirrors the Type enum in the SharedResources API, kept as a
// separate domain type so this package does not depend on the generated
// protobuf bindings. Most values are Linux namespace types; TypeDevShm is
// not — see its own comment.
type Type int

const (
	// TypeNamespaceIPC is an IPC namespace.
	TypeNamespaceIPC Type = iota + 1
	// TypeNamespacePID is a PID namespace.
	TypeNamespacePID
	// TypeNamespaceNetwork is a network namespace.
	TypeNamespaceNetwork
	// TypeDevShm is not an OCI namespace: it is a per-group tmpfs that
	// sharing containers bind-mount their /dev/shm onto. See the package
	// doc comment for why it lives here.
	TypeDevShm
)

// String implements fmt.Stringer.
func (t Type) String() string {
	switch t {
	case TypeNamespaceIPC:
		return "ipc"
	case TypeNamespacePID:
		return "pid"
	case TypeNamespaceNetwork:
		return "network"
	case TypeDevShm:
		return "devshm"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// dir returns the directory resources of this type are pinned in. The
// layout matches the convention used by iproute2 and containerd's CRI
// plugin for named network namespaces (/run/netns/<name>), extended to the
// other types.
func (t Type) dir() (string, error) {
	switch t {
	case TypeNamespaceIPC:
		return "/run/ipcns", nil
	case TypeNamespacePID:
		return "/run/pidns", nil
	case TypeNamespaceNetwork:
		return "/run/netns", nil
	case TypeDevShm:
		return "/run/devshm", nil
	default:
		return "", fmt.Errorf("unknown resource type %d: %w", int(t), errdefsInvalidArgument)
	}
}

// errdefsInvalidArgument is matched by the service layer to map validation
// failures onto an InvalidArgument status without importing errdefs here.
var errdefsInvalidArgument = errors.New("invalid argument")

// ErrInvalidArgument is returned for a malformed group id or an unknown
// resource type.
var ErrInvalidArgument = errdefsInvalidArgument

// key identifies one managed resource.
type key struct {
	id  string
	typ Type
}

// entry is the state of one managed resource. Once created, path is set and
// err is nil; if creation failed, err is set and is returned to every later
// caller rather than silently retrying a broken setup.
type entry struct {
	path string
	err  error
	// anchor is the process holding a PID namespace open. Only set for
	// TypeNamespacePID; a PID namespace is destroyed by the kernel as soon
	// as its PID 1 exits, so unlike the other types it cannot be kept
	// alive by a bind mount alone.
	anchor *os.Process
}

// Manager creates shared resources on demand and remembers them, so that
// repeated requests for the same (id, type) return the same path without
// doing the work again. A single Manager is meant to be shared for the
// lifetime of one vminitd process.
//
// Safe for concurrent use.
type Manager struct {
	mu sync.Mutex
	ns map[key]*entry
}

// Create creates each of types for the group id that does not exist yet, and
// returns the guest path of every requested type. Duplicate types in the
// request are collapsed. On failure no partial result is returned, but any
// resources created earlier in the call are kept and will be reused by a
// later call.
//
// devShmSizeBytes is only consulted when types includes TypeDevShm and no
// devshm resource exists yet for id; see createDevShm. It is ignored
// otherwise, including when a devshm resource for id already exists — the
// size the first caller supplied wins.
func (m *Manager) Create(ctx context.Context, id string, types []Type, devShmSizeBytes int64) (map[Type]string, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	wanted, err := dedupe(types)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	paths := make(map[Type]string, len(wanted))
	for _, typ := range wanted {
		e, err := m.ensureLocked(ctx, id, typ, devShmSizeBytes)
		if err != nil {
			return nil, err
		}
		paths[typ] = e.path
	}
	return paths, nil
}

// Delete removes each of types for the group id. An empty types list deletes
// every resource belonging to id. Deleting something that does not exist is
// not an error.
func (m *Manager) Delete(ctx context.Context, id string, types []Type) error {
	if err := validateID(id); err != nil {
		return err
	}
	wanted, err := dedupe(types)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(wanted) == 0 {
		for k := range m.ns {
			if k.id == id {
				wanted = append(wanted, k.typ)
			}
		}
	}

	var errs []error
	for _, typ := range wanted {
		if err := m.deleteLocked(ctx, id, typ); err != nil {
			errs = append(errs, fmt.Errorf("delete %s resource: %w", typ, err))
		}
	}
	return errors.Join(errs...)
}

// ensureLocked returns the entry for (id, typ), creating the resource if it
// does not exist. m.mu must be held.
func (m *Manager) ensureLocked(ctx context.Context, id string, typ Type, devShmSizeBytes int64) (*entry, error) {
	k := key{id: id, typ: typ}
	if e, ok := m.ns[k]; ok {
		if e.err != nil {
			return nil, e.err
		}
		return e, nil
	}

	dir, err := typ.dir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id)

	e := &entry{path: path}
	switch typ {
	case TypeNamespaceNetwork:
		e.err = createNetwork(ctx, id, path)
	case TypeNamespaceIPC:
		e.err = createIPC(ctx, path)
	case TypeNamespacePID:
		e.anchor, e.err = createPID(ctx, path)
	case TypeDevShm:
		e.err = createDevShm(path, devShmSizeBytes)
	default:
		return nil, fmt.Errorf("unknown resource type %d: %w", int(typ), ErrInvalidArgument)
	}
	if e.err != nil {
		e.err = fmt.Errorf("create %s resource %q: %w", typ, path, e.err)
	}

	if m.ns == nil {
		m.ns = make(map[key]*entry)
	}
	m.ns[k] = e

	if e.err != nil {
		return nil, e.err
	}
	log.G(ctx).WithFields(log.Fields{
		"id":   id,
		"type": typ.String(),
		"path": path,
	}).Debug("created shared resource")
	return e, nil
}

// deleteLocked tears down the resource for (id, typ). m.mu must be held.
func (m *Manager) deleteLocked(ctx context.Context, id string, typ Type) error {
	k := key{id: id, typ: typ}
	e, ok := m.ns[k]
	if !ok {
		return nil
	}
	delete(m.ns, k)

	// A failed creation left nothing behind worth unmounting beyond the
	// placeholder, which unpin handles.
	if e.anchor != nil {
		// Killing PID 1 is what actually destroys a PID namespace; the
		// kernel then reaps everything else in it. Wait so the anchor does
		// not linger as a zombie child of vminitd.
		if err := e.anchor.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.G(ctx).WithError(err).WithField("id", id).Warn("failed to kill namespace anchor")
		}
	}
	return unpin(e.path)
}

// createNetwork creates a network namespace pinned at path and brings up its
// loopback interface.
//
// This is the "persistent namespace" technique containerd's CRI plugin uses
// on the host: a dedicated goroutine locks itself to an OS thread, unshares
// on that thread, and bind-mounts the thread's namespace file to a
// well-known path. The bind mount is what keeps the namespace alive, so the
// creating goroutine does not need to stay running afterwards. Go retires
// the locked OS thread when the goroutine exits (Go 1.10+), so leaving it
// locked does not poison the thread pool.
//
// netns.NewNamed pins at /run/netns/<name>, which is exactly the layout
// Type.dir uses for TypeNamespaceNetwork, so id is passed as the name.
func createNetwork(ctx context.Context, id, path string) error {
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Intentionally no UnlockOSThread: this thread's namespace has been
		// replaced and must never be reused for unrelated work.

		nsh, err := netns.NewNamed(id)
		if err != nil {
			errCh <- fmt.Errorf("create named netns: %w", err)
			return
		}
		// NewNamed returns an open handle to the new namespace. The bind
		// mount it made is what keeps the namespace alive, so this
		// descriptor is redundant and would otherwise be leaked for the
		// lifetime of the process.
		defer nsh.Close()

		// NewNamed leaves this locked thread inside the new namespace, so
		// plain netlink calls operate on it without needing a NewHandleAt.
		link, err := netlink.LinkByName("lo")
		if err != nil {
			errCh <- fmt.Errorf("lookup lo: %w", err)
			return
		}
		if err := netlink.LinkSetUp(link); err != nil {
			errCh <- fmt.Errorf("bring up lo: %w", err)
			return
		}
		errCh <- nil
	}()
	if err := <-errCh; err != nil {
		// netns.NewNamed may have created the bind-mount target before
		// failing; make sure a later attempt is not blocked by it.
		_ = unpin(path)
		return err
	}
	return nil
}

// createIPC creates an IPC namespace pinned at path, using the same
// locked-thread technique as createNetwork. unshare(CLONE_NEWIPC), unlike
// CLONE_NEWPID, takes effect on the calling thread immediately, so the
// thread's own namespace file is the one to bind-mount.
func createIPC(_ context.Context, path string) error {
	if err := pin(path); err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Intentionally no UnlockOSThread: see createNetwork.

		if err := unix.Unshare(unix.CLONE_NEWIPC); err != nil {
			errCh <- fmt.Errorf("unshare CLONE_NEWIPC: %w", err)
			return
		}
		src := fmt.Sprintf("/proc/self/task/%d/ns/ipc", unix.Gettid())
		if err := unix.Mount(src, path, "", unix.MS_BIND, ""); err != nil {
			errCh <- fmt.Errorf("bind mount %s: %w", src, err)
			return
		}
		errCh <- nil
	}()
	if err := <-errCh; err != nil {
		_ = unpin(path)
		return err
	}
	return nil
}

// createPID creates a PID namespace pinned at path and returns the anchor
// process holding it open.
//
// A PID namespace cannot be created the way createNetwork and createIPC
// create theirs. unshare(CLONE_NEWPID) does not move the caller into the new
// namespace; it only arranges for the caller's next child to become PID 1 of
// one. A thread can therefore never be the namespace's PID 1, and
// /proc/self/ns/pid_for_children has no value to bind-mount until that first
// child exists. Worse, the kernel destroys a PID namespace as soon as its
// PID 1 exits, and no further processes can be created in it after that, so
// a bind mount cannot substitute for a live process the way it can for the
// other types. Hence a real anchor process.
func createPID(ctx context.Context, path string) (*os.Process, error) {
	if err := pin(path); err != nil {
		return nil, err
	}

	cmd := exec.Command(anchorCommand[0], anchorCommand[1:]...) //nolint:gosec // fixed command, not caller-controlled
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start anchor: %w", err)
	}

	src := fmt.Sprintf("/proc/%d/ns/pid", cmd.Process.Pid)
	if err := unix.Mount(src, path, "", unix.MS_BIND, ""); err != nil {
		// Nothing else will ever wait on or kill this process, so it would
		// run for the rest of the VM's lifetime. Tear it down here, and do
		// so synchronously so no goroutine is left blocked on a Wait that
		// nothing else is coordinating with.
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.G(ctx).WithError(killErr).Warn("failed to kill namespace anchor after mount failure")
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			log.G(ctx).WithError(waitErr).Debug("namespace anchor wait after mount failure")
		}
		return nil, fmt.Errorf("bind mount %s: %w", src, err)
	}

	// Reap the anchor once it exits so it does not linger as a zombie child
	// of vminitd. Normally it only exits when Delete kills it, or never.
	// Started only after the bind mount succeeded, so the failure path above
	// owns the Wait in that case.
	go func() {
		if err := cmd.Wait(); err != nil {
			log.G(ctx).WithError(err).Debug("namespace anchor exited")
		}
	}()

	return cmd.Process, nil
}

// defaultDevShmSizeBytes is used when devShmSizeBytes is not positive. The
// host-side caller always computes a real size from the sharing container's
// own CRI-provided /dev/shm mount options (falling back to its own default
// of the same value if unspecified), so this should not normally be
// reached; it exists purely as a defensive floor so a tmpfs is never
// created with a nonsensical size (in particular, "size=0" would make the
// tmpfs unusable — every write to it would fail with ENOSPC).
const defaultDevShmSizeBytes = 64 * 1024 * 1024

// devShmMountFlags matches the nosuid/noexec/nodev flags CRI itself
// requests on a container's own (unshared) /dev/shm tmpfs mount, so sharing
// does not weaken those properties.
const devShmMountFlags = unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV

// createDevShm creates a real, sized tmpfs pinned at path, for sharing
// containers to bind-mount their /dev/shm onto.
//
// Unlike the namespace types, this needs no bind-mount-of-a-namespace-file
// trick and no anchor process: it is a plain tmpfs mount, and the mount
// itself is what needs to stay alive, which the kernel already guarantees
// for as long as anything references it. It is mounted here, in the
// guest's own root mount namespace under /run, rather than inside the
// virtiofs-backed "containers" tree used for rootfs/volumes: that keeps
// this tmpfs real guest RAM with a real, kernel-enforced size limit,
// rather than something backed by host disk and reached over virtiofs. A
// member container's own crun bind-mounts this path directly, the same
// way it already joins /run/netns/<id> and /run/ipcns/<id>.
func createDevShm(path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		sizeBytes = defaultDevShmSizeBytes
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}

	data := fmt.Sprintf("size=%d,mode=1777", sizeBytes)
	if err := unix.Mount("tmpfs", path, "tmpfs", devShmMountFlags, data); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("mount tmpfs: %w", err)
	}
	return nil
}

// pin creates the empty file a namespace is bind-mounted onto, along with its
// parent directory.
func pin(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return fmt.Errorf("create bind-mount target: %w", err)
	}
	return f.Close()
}

// unpin unmounts a pinned resource and removes its bind-mount target. It is
// idempotent: an already-unmounted or already-removed path is not an error.
func unpin(path string) error {
	// The unmount result is deliberately ignored. There are several benign
	// reasons it fails — the path was pinned but never mounted onto (EINVAL,
	// or EPERM for an unprivileged caller), or it is already gone (ENOENT) —
	// and distinguishing them from a real failure by errno alone is not
	// reliable. The removal below is the actual check: if the resource is
	// still mounted here, it fails with EBUSY and that is reported.
	_ = unix.Unmount(path, 0)

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// validateID rejects group ids that are empty or that could escape the
// per-type directory once joined onto it. The id arrives over RPC and is
// used to build a filesystem path, so it is never trusted.
func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("resource group id is required: %w", ErrInvalidArgument)
	}
	if id == "." || id == ".." || strings.ContainsRune(id, os.PathSeparator) || strings.ContainsRune(id, 0) {
		return fmt.Errorf("invalid resource group id %q: %w", id, ErrInvalidArgument)
	}
	return nil
}

// dedupe removes repeated types, preserving first-seen order, and rejects
// unknown ones.
func dedupe(types []Type) ([]Type, error) {
	out := make([]Type, 0, len(types))
	seen := make(map[Type]struct{}, len(types))
	for _, typ := range types {
		if _, err := typ.dir(); err != nil {
			return nil, err
		}
		if _, ok := seen[typ]; ok {
			continue
		}
		seen[typ] = struct{}{}
		out = append(out, typ)
	}
	return out, nil
}
