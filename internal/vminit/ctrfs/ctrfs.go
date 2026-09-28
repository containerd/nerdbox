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

// Package ctrfs provides access to a container's filesystem as the container
// itself sees it.
//
// A container's mounts are applied by the OCI runtime inside the container's
// own mount namespace. The bundle's rootfs directory, which is what vminitd
// sees in its own mount namespace, therefore only backs the paths that no
// mount covers: every bind, volume, tmpfs and overlay destination is shadowed
// by an empty directory. Reading or writing through the bundle rootfs silently
// targets those shadowed entries rather than the mounted content.
//
// This package resolves paths through the container's mount namespace instead,
// so the kernel performs the same mount resolution the container's own
// processes get, for every mount type, in the correct stacking order.
//
// Only the mount namespace is joined, which makes this close to but not the
// same as running inside the container. Operations keep vminitd's remaining
// namespaces and its credentials: root, every capability, no seccomp filter
// and no cgroup limits. They can therefore read and write what the container's
// own processes cannot, and anything the kernel derives from a namespace
// reflects vminitd's rather than the container's, so /proc/sys/net reports the
// VM's network settings and /proc/self does not resolve at all. Resolution
// cannot ascend above the container's root, but procfs magic links such as
// /proc/<pid>/root are not resolution, and where those lead depends on whether
// the container was given its own PID namespace.
package ctrfs

import (
	"fmt"
	"sync"

	"github.com/containerd/errdefs"
)

// Registry tracks a pinned mount namespace per container, so a container's
// filesystem stays reachable for as long as the container exists.
//
// A mount namespace lives while it has at least one reference: a member
// process, an open file descriptor on /proc/<pid>/ns/mnt, or a bind mount of
// that path. Relying on a member process would restrict access to running
// containers, because the namespace and its whole mount tree are torn down as
// soon as the last process in it exits, which happens on its own for any
// container whose main process returns. Holding a file descriptor keeps the
// namespace and its mounts intact after that, so a container that has exited
// but not yet been deleted remains readable.
//
// A Registry is safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	handles map[string]*Handle
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{handles: make(map[string]*Handle)}
}

// Add pins the mount namespace of pid and records it under the given container
// ID. The namespace stays pinned until Release or Close.
//
// Add must be called while pid is still alive, which for a container means any
// time after the OCI runtime has created it. The container's mounts are
// already in place at that point, before its init process has been started.
//
// Adding an ID that is already present replaces and releases the previous
// pin, so a recreated container does not inherit a stale namespace.
func (r *Registry) Add(id string, pid int) error {
	if id == "" {
		return errdefs.ErrInvalidArgument.WithMessage("container ID is required")
	}
	if pid <= 0 {
		return errdefs.ErrInvalidArgument.WithMessage(fmt.Sprintf("invalid pid %d for container %s", pid, id))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Whatever this ID referred to before is dropped first, and stays
	// dropped if the pin below fails. The caller goes on to use the ID for
	// the new container either way, and resolving that container's paths
	// through its predecessor's namespace would be worse than not resolving
	// them at all.
	if prev, ok := r.handles[id]; ok {
		prev.Close()
		delete(r.handles, id)
	}

	h, err := pin(pid)
	if err != nil {
		return fmt.Errorf("failed to pin mount namespace of container %s: %w", id, err)
	}
	r.handles[id] = h
	return nil
}

// Release drops the pin recorded for the given container ID, allowing the
// kernel to tear the namespace down once nothing else references it. Releasing
// an ID that is not present is not an error, so Release is safe to call on
// cleanup paths that may run more than once.
func (r *Registry) Release(id string) error {
	r.mu.Lock()
	h, ok := r.handles[id]
	delete(r.handles, id)
	r.mu.Unlock()

	if !ok {
		return nil
	}
	return h.Close()
}

// Do runs fn with the given container's filesystem as the root directory, so
// paths inside fn resolve as the container resolves them: to mounted content
// rather than to the entries the container's mounts shadow in the bundle
// rootfs.
//
// See Handle.Do for the constraints on fn. In particular fn must do all of its
// filesystem work on the calling goroutine.
func (r *Registry) Do(id string, fn func() error) error {
	r.mu.Lock()
	h, ok := r.handles[id]
	r.mu.Unlock()

	if !ok {
		return errdefs.ErrNotFound.WithMessage(fmt.Sprintf("no mount namespace recorded for container %s", id))
	}
	return h.Do(fn)
}

// Close releases every reference held by the registry.
func (r *Registry) Close() error {
	r.mu.Lock()
	handles := r.handles
	r.handles = make(map[string]*Handle)
	r.mu.Unlock()

	var firstErr error
	for _, h := range handles {
		if err := h.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
