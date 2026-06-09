//go:build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0
*/

package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	api "github.com/containerd/nerdbox/api/services/mount/v1"
)

// MountInContainer enters the mount namespace of the container identified by
// `container_pid` and performs the requested bind mount inside it. This is the
// runtime counterpart of nerdbox's create-time bind-mount transformation —
// callers feed it a source that already exists in the VM (typically under a
// virtiofs share mounted at /mnt/<tag>) and a target inside the container.
//
// The mount namespace switch (setns(2)) is per-thread on Linux. We lock the
// current goroutine to its OS thread for the duration of the call and
// deliberately never UnlockOSThread on the success path: an OS thread that has
// been moved into a foreign mount namespace must never be returned to the
// general goroutine pool, because future goroutines scheduled on it would
// silently inherit that namespace. The runtime reaps the abandoned thread
// once this goroutine exits.
//
// Bind-only — the request's `Type` and any options other than `ro`/`rw` are
// ignored. Mount options recognised:
//
//	"ro"  — read-only bind (MS_RDONLY remount after the initial MS_BIND)
//	"rw"  — explicit read-write (default; no-op)
//
// The target's parent directory is created with mkdir -p semantics. The
// target file/directory itself is created if missing (a regular file when the
// source is a regular file, a directory otherwise) so callers don't have to
// pre-stage container paths.
func (s *service) MountInContainer(ctx context.Context, r *api.MountInContainerRequest) (*api.MountInContainerResponse, error) {
	if r.ContainerPid == 0 {
		return nil, errgrpc.ToGRPC(fmt.Errorf("container_pid is required"))
	}
	if r.Mount == nil || r.Mount.Source == "" || r.Mount.Target == "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("mount.source and mount.target are required"))
	}

	readOnly := false
	for _, opt := range r.Mount.Options {
		switch opt {
		case "ro":
			readOnly = true
		case "rw":
			readOnly = false
		}
	}

	log.G(ctx).WithFields(log.Fields{
		"pid":      r.ContainerPid,
		"source":   r.Mount.Source,
		"target":   r.Mount.Target,
		"readonly": readOnly,
	}).Info("bind-mount inside container namespace")

	args := []string{"mount", strconv.Itoa(int(r.ContainerPid)), r.Mount.Source, r.Mount.Target}
	if readOnly {
		args = append(args, "ro")
	}
	if err := runMountHelper(ctx, args); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.MountInContainerResponse{}, nil
}

// UnmountInContainer enters the mount namespace of the container identified
// by `container_pid` and calls umount2(target). Idempotent — an unmount of a
// path that isn't currently a mount returns success so callers don't have to
// pre-check.
func (s *service) UnmountInContainer(ctx context.Context, r *api.UnmountInContainerRequest) (*api.UnmountInContainerResponse, error) {
	if r.ContainerPid == 0 {
		return nil, errgrpc.ToGRPC(fmt.Errorf("container_pid is required"))
	}
	if r.Target == "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("target is required"))
	}

	log.G(ctx).WithFields(log.Fields{
		"pid":    r.ContainerPid,
		"target": r.Target,
	}).Info("unmount inside container namespace")

	args := []string{"umount", strconv.Itoa(int(r.ContainerPid)), r.Target}
	if err := runMountHelper(ctx, args); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.UnmountInContainerResponse{}, nil
}

// nsmountHelperPath is the absolute path of the static C helper in the
// initrd. setns(CLONE_NEWNS) requires a single-threaded calling process, but
// the Go runtime is multi-threaded by the time vminitd's plugin handlers
// run (sysmon starts before main()), so we shell out to a non-Go binary.
const nsmountHelperPath = "/sbin/nerdbox-nsmount"

// runMountHelper invokes the static C helper at [nsmountHelperPath]. Args
// follow the helper's argv protocol (see cmd/nsmount-helper/nsmount.c).
// A non-zero exit code is surfaced as an error with the helper's stderr.
func runMountHelper(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, nsmountHelperPath, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return fmt.Errorf("mount helper failed: %w", err)
	}
	return fmt.Errorf("mount helper failed: %s", msg)
}

// doUnmount is the unmount counterpart of [doBindMount]. EINVAL/ENOENT are
// treated as success so umount stays idempotent — the in-container target
// either was never a mount point or has already been cleaned up.
func doUnmount(target string) error {
	if err := unix.Unmount(target, 0); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("umount %s: %w", target, err)
	}
	return nil
}

// enterMountNS opens /proc/<pid>/ns/mnt and calls setns(2) with
// CLONE_NEWNS on the current OS thread. Returns any error from open or setns.
func enterMountNS(pid int) error {
	nsPath := fmt.Sprintf("/proc/%d/ns/mnt", pid)
	f, err := os.Open(nsPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", nsPath, err)
	}
	defer f.Close()
	if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns %s: %w", nsPath, err)
	}
	return nil
}

// doBindMount creates target (and any missing parents) and performs a bind
// mount from source to target. When readOnly is true, follows up with a
// remount carrying MS_RDONLY — the kernel ignores read-only flags on the
// initial bind mount, so the remount is needed.
func doBindMount(source, target string, readOnly bool) error {
	st, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat source %s: %w", source, err)
	}

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", parent, err)
	}

	if st.IsDir() {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("mkdir target %s: %w", target, err)
		}
	} else {
		// Bind-mounting a regular file onto a regular file requires the
		// target to exist as a file. Touch it if missing.
		if _, err := os.Stat(target); os.IsNotExist(err) {
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("touch target %s: %w", target, err)
			}
			_ = f.Close()
		} else if err != nil {
			return fmt.Errorf("stat target %s: %w", target, err)
		}
	}

	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount %s → %s: %w", source, target, err)
	}
	if readOnly {
		// Remount RO. MS_BIND alone ignores RO; MS_REMOUNT|MS_BIND|MS_RDONLY is the canonical
		// way to make a bind mount read-only.
		if err := unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
			// Best-effort: try to undo the bind on failure so the caller
			// doesn't end up with a RW mount when they asked for RO.
			_ = unix.Unmount(target, 0)
			return fmt.Errorf("remount %s read-only: %w", target, err)
		}
	}
	return nil
}

// Compile-time sanity: the service must satisfy the generated interface,
// including the new MountInContainer and UnmountInContainer methods.
var _ api.TTRPCMountService = (*service)(nil)
