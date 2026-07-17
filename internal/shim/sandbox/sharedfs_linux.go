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

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/containerd/nerdbox/internal/mountutil"
)

// shareRootfs is the Linux implementation backing SharedFS.ShareRootfs. See
// its doc comment in sharedfs.go for the full contract.
func (s *SharedFS) shareRootfs(ctx context.Context, containerID string, mounts []*types.Mount) (guestPath string, err error) {
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

// shareVolume is the Linux implementation backing SharedFS.ShareVolume. See
// its doc comment in sharedfs.go for the full contract.
func (s *SharedFS) shareVolume(ctx context.Context, containerID string, n int, hostSource string, isDir bool) (guestPath string, err error) {
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

// unshare is the Linux implementation backing SharedFS.Unshare. See its doc
// comment in sharedfs.go for the full contract.
func (s *SharedFS) unshare(ctx context.Context, containerID string) error {
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

// unshareAll is the Linux implementation backing SharedFS.UnshareAll. See
// its doc comment in sharedfs.go for the full contract.
func (s *SharedFS) unshareAll(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.mounts))
	for id := range s.mounts {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	var errs []error
	for _, id := range ids {
		if err := s.unshare(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("unshare all: %v", errs)
	}
	return nil
}
