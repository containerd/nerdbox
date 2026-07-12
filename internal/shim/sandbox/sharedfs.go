// Copyright The containerd Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/containerd/nerdbox/internal/mountutil"
)

// SharedFSTag is the virtiofs share tag used for the per-sandbox container
// filesystem tree. The guest mounts this at GuestContainersDir.
const SharedFSTag = "containers"

// GuestContainersDir is the path in the guest where the shared filesystem
// is mounted. Per-container rootfs and volumes live under:
//
//	/run/containers/<container-id>/rootfs
//	/run/containers/<container-id>/volumes/<n>
//
// /run is backed by a tmpfs in the guest so the mount point is always
// writable even on the read-only erofs base rootfs.
const GuestContainersDir = "/run/containers"

// SharedFS manages the host-side directory tree shared with the VM via a
// single virtiofs mount. It creates per-container subdirectories, assembles
// the container rootfs from snapshotter-provided mounts, and tears everything
// down on container delete.
//
// The root directory is <sandbox-state>/containers. It is added to the VM as
// a virtiofs share with tag "containers" before the VM starts and must not be
// modified until after the VM shuts down.
//
// Thread-safe: all exported methods may be called concurrently.
type SharedFS struct {
	mu   sync.Mutex
	root string // host path of the shared dir
	// mounts tracks the mount points we created per container so we can
	// unmount them precisely on Unshare.
	mounts map[string][]string // containerID -> ordered list of host mount points
}

// NewSharedFS creates a SharedFS rooted at <stateDir>/containers.
// The directory is created if it does not exist.
func NewSharedFS(stateDir string) (*SharedFS, error) {
	root := filepath.Join(stateDir, "containers")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create shared containers dir %s: %w", root, err)
	}
	return &SharedFS{
		root:   root,
		mounts: make(map[string][]string),
	}, nil
}

// Root returns the host-side root of the shared filesystem. This path is
// passed to the VM as the backing directory for the virtiofs share.
func (s *SharedFS) Root() string {
	return s.root
}

// GuestRootfsPath returns the in-guest path of the container's assembled
// rootfs, suitable for passing to the guest Task.Create as the rootfs source.
func GuestRootfsPath(containerID string) string {
	return filepath.Join(GuestContainersDir, containerID, "rootfs")
}

// GuestVolumePath returns the in-guest path for volume mount n of the given
// container (0-indexed), suitable for bind-mounting into the container.
func GuestVolumePath(containerID string, n int) string {
	return filepath.Join(GuestContainersDir, containerID, "volumes", fmt.Sprintf("%d", n))
}

// ShareRootfs resolves the container rootfs from the given containerd mount
// specs by executing them on the host inside the shim's mount namespace, and
// exposes the result in the shared filesystem tree so the guest can access it
// at GuestRootfsPath(containerID).
//
// The mounts parameter is exactly what containerd passes in the Task.Create
// request — the same set of specs the snapshotter would normally apply
// locally. We execute them here inside the shim's private mount namespace so
// that cleanup is automatic when the shim process exits.
//
// The rootfs is exposed at the correct guest path via a real mount: a kernel
// bind mount, an overlay mount, a FUSE mount, or any other type that
// mountutil.All can apply.  If the mount cannot be established the container
// run fails — there is no fallback to file copies or hard links, which would
// silently produce incorrect behaviour (dirty-page accumulation, cross-device
// failures, and loss of file-system metadata).
//
// Returns the in-guest path where the assembled rootfs will be accessible.
func (s *SharedFS) ShareRootfs(ctx context.Context, containerID string, mounts []*types.Mount) (guestPath string, err error) {
	hostRootfs := filepath.Join(s.root, containerID, "rootfs")

	if len(mounts) == 0 {
		// No mounts: create an empty rootfs target directory.
		if err := os.MkdirAll(hostRootfs, 0o755); err != nil {
			return "", fmt.Errorf("create rootfs dir %s: %w", hostRootfs, err)
		}
		return GuestRootfsPath(containerID), nil
	}

	if err := os.MkdirAll(hostRootfs, 0o755); err != nil {
		return "", fmt.Errorf("create rootfs dir %s: %w", hostRootfs, err)
	}

	// Intermediate directory for chained mounts (all but the last mount in
	// the list are mounted under here; the last is mounted directly at
	// hostRootfs). This mirrors the legacy/plain-container path in
	// internal/shim/task/mount_linux.go, which uses mountutil.All the same
	// way for the same reason: it, not the generic containerd mount.All,
	// understands nerdbox's custom "format/" and "mkdir/" mount option
	// prefixes (e.g. X-containerd.mkdir.path=...) used to build overlay
	// upper/work directories before mounting.
	lmounts := filepath.Join(s.root, containerID, "mnt")
	if err := os.MkdirAll(lmounts, 0o755); err != nil {
		return "", fmt.Errorf("create intermediate mount dir %s: %w", lmounts, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"container": containerID,
		"mounts":    mounts,
		"target":    hostRootfs,
	}).Debug("assembling container rootfs on host")

	if err := mountutil.All(ctx, hostRootfs, lmounts, mounts); err != nil {
		return "", fmt.Errorf("mount container rootfs for %s: %w", containerID, err)
	}

	// mountutil.All mounts every entry in mounts: all but the last under
	// lmounts/<index>, and the last at hostRootfs. Track every mount point
	// it created (not just hostRootfs) so Unshare tears all of them down —
	// otherwise the intermediate lowerdir mounts backing the final overlay
	// would leak. Order matters: hostRootfs (the outermost mount, depending
	// on the others) must be unmounted before its lower layers, so it is
	// appended last and Unshare's reverse-order unmount hits it first.
	mountPts := make([]string, 0, len(mounts))
	for i := range mounts {
		if i < len(mounts)-1 {
			mountPts = append(mountPts, filepath.Join(lmounts, fmt.Sprintf("%d", i)))
		}
	}
	mountPts = append(mountPts, hostRootfs)

	s.mu.Lock()
	s.mounts[containerID] = append(s.mounts[containerID], mountPts...)
	s.mu.Unlock()

	return GuestRootfsPath(containerID), nil
}

// ShareVolume bind-mounts hostSource (a host path from an OCI "bind" mount
// in a member container's spec) into the shared filesystem tree at
// GuestVolumePath(containerID, n), and returns that guest path.
//
// This exists because a member container's volume mounts cannot use the
// same mechanism as the legacy/plain-container path (internal/shim/task's
// bindMounter, which shares each bind mount as its own new virtiofs tag):
// by the time a member container is created the sandbox's VM is already
// running, and virtio-fs shares cannot be hot-added after boot. Instead,
// the host source is bind-mounted directly into the shared directory tree
// that is already exposed to the guest via the single, persistent,
// pre-boot "containers" virtiofs share (the same one ShareRootfs uses) —
// so the guest sees the volume's content immediately, with no new virtiofs
// device and no additional guest-side Mount.MountAll step required at all.
//
// isDir must reflect whether hostSource is a directory or a regular file:
// unlike a virtiofs share (which must be a directory), a plain bind mount
// can target either, but the mountpoint placeholder this function creates
// must match (a directory for a directory bind mount, an empty regular
// file for a file bind mount) or the mount(2) call fails.
//
// This mount is always read-write and recursive (rbind), regardless of
// what the container's OCI spec requests for the volume: it exists purely
// to expose hostSource's content (including any nested mounts under it) to
// the guest. The caller (sandboxVolumeMounter) only rewrites the spec's
// mount Source, leaving Options untouched, so the actual container-visible
// read-only/recursion semantics are enforced exactly once, by the guest's
// own OCI runtime (crun) performing its own bind mount from
// GuestVolumePath into the container using those original options. Making
// *this* mount read-only too would be actively wrong, not just redundant:
// a recursive read-only bind mount sets Linux's MNT_LOCKED on every mount
// in the hierarchy, and that lock cannot be undone by any later mount
// (including crun's, or a fresh mount the container creates inside it) —
// so a container-requested *non-recursive* read-only volume would become
// unintentionally, unremovably read-only all the way down.
func (s *SharedFS) ShareVolume(ctx context.Context, containerID string, n int, hostSource string, isDir bool) (guestPath string, err error) {
	target := filepath.Join(s.root, containerID, "volumes", fmt.Sprintf("%d", n))

	if isDir {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return "", fmt.Errorf("create volume dir %s: %w", target, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", fmt.Errorf("create volume parent dir for %s: %w", target, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE, 0o644)
		if err != nil {
			return "", fmt.Errorf("create volume file placeholder %s: %w", target, err)
		}
		f.Close()
	}

	m := mount.Mount{Type: "bind", Source: hostSource, Options: []string{"rbind", "rw"}}
	if err := m.Mount(target); err != nil {
		return "", fmt.Errorf("bind mount volume %s -> %s: %w", hostSource, target, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"container": containerID,
		"n":         n,
		"source":    hostSource,
		"target":    target,
	}).Debug("shared container volume mount")

	s.mu.Lock()
	s.mounts[containerID] = append(s.mounts[containerID], target)
	s.mu.Unlock()

	return GuestVolumePath(containerID, n), nil
}

// Unshare removes all host-side mounts created for containerID and deletes
// its subtree under the shared directory. It is idempotent.
func (s *SharedFS) Unshare(ctx context.Context, containerID string) error {
	s.mu.Lock()
	mountPts := s.mounts[containerID]
	delete(s.mounts, containerID)
	s.mu.Unlock()

	var errs []error

	// Unmount in reverse order (deepest first).
	for i := len(mountPts) - 1; i >= 0; i-- {
		pt := mountPts[i]
		log.G(ctx).WithFields(log.Fields{
			"container": containerID,
			"target":    pt,
		}).Debug("unmounting container rootfs")
		// MNT_DETACH performs a lazy unmount: the mount is detached from
		// the filesystem hierarchy immediately even if the directory is
		// still in use (e.g. while virtiofs is serving files from it).
		// The mount is cleaned up when all references are dropped.
		if err := mount.UnmountAll(pt, unix.MNT_DETACH); err != nil {
			log.G(ctx).WithError(err).WithField("target", pt).Warn("failed to unmount rootfs")
			errs = append(errs, fmt.Errorf("unmount %s: %w", pt, err))
		}
	}

	// Best-effort removal of the container subtree.
	ctrDir := filepath.Join(s.root, containerID)
	if err := os.RemoveAll(ctrDir); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).WithField("dir", ctrDir).Warn("failed to remove container shared dir")
	}

	if len(errs) > 0 {
		return fmt.Errorf("unshare %s: %w", containerID, errs[0])
	}
	return nil
}

// UnshareAll removes all containers. Called on sandbox shutdown after the VM
// has stopped so host-side cleanup does not race live mounts.
func (s *SharedFS) UnshareAll(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.mounts))
	for id := range s.mounts {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	var errs []error
	for _, id := range ids {
		if err := s.Unshare(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("unshare all: %v", errs)
	}
	return nil
}
