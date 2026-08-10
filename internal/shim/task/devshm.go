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

package task

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// defaultDevShmSize is used when a /dev/shm mount's own options don't
// specify a size (or specify one this package can't parse). It matches the
// size Kubernetes itself defaults to when a pod sets no explicit limit.
const defaultDevShmSize = 64 * 1024 * 1024

// shareDevShmMounter is a bundle.Transformer for sandbox member containers.
// It gives member containers that share an IPC namespace a working
// /dev/shm: containerd sends every member container of a pod the same,
// independent `{Destination: "/dev/shm", Type: "tmpfs", Source: "shm"}`
// mount (confirmed via `crictl inspect` against a live CRI pod) — the same
// spec Kubernetes' non-VM runtimes rely on turning into a shared mount
// themselves, but on nerdbox's guest kernel it just makes each container's
// own crun create an independent, private tmpfs. Sidecars that share SysV
// IPC (via the pod's shared IPC namespace) but write POSIX shared memory
// through /dev/shm therefore don't actually see each other's writes.
//
// FromBundle rewrites that mount, when present on a container that is
// sharing IPC, into a "bind" mount of the sandbox's shared /dev/shm tmpfs
// (getDevShm, backed by the guest's SharedResources service — see
// internal/vminit/sharedresources.TypeDevShm). This is a real, sized tmpfs
// living entirely in the guest's own root mount namespace, not something
// shared through the host/virtiofs like sandboxVolumeMounter's ordinary
// bind mounts, so it is real guest RAM with a real, kernel-enforced size
// limit rather than something backed by host disk. Must therefore run
// after sandboxVolumeMounter.FromBundle, which only rewrites mounts that
// already have Type "bind" — this mount is still "tmpfs" until this
// transformer runs, so it is untouched by that pass, and the guest path
// this produces needs no further host-side sharing at all.
//
// A container with no shared IPC namespace (NamespaceMode_CONTAINER, the
// default for a pod that never asks for IPC sharing) is left alone: its
// /dev/shm mount passes through unchanged, and it gets the same private
// tmpfs it would have gotten anyway.
type shareDevShmMounter struct {
	containerID string
	// getDevShmFn is normally (*sharedResources).getDevShm; a field
	// (rather than a *sharedResources) so tests can substitute a fake
	// without needing a real guest TTRPC connection.
	getDevShmFn func(ctx context.Context, sizeBytes int64) (string, error)
}

// FromBundle implements the rewrite described in the type doc comment.
func (d *shareDevShmMounter) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	if !containerSharesIPC(b) {
		return nil
	}

	for i, m := range b.Spec.Mounts {
		if m.Destination != "/dev/shm" || m.Type == "bind" {
			continue
		}

		guestPath, err := d.getDevShmFn(ctx, devShmSize(m.Options))
		if err != nil {
			return fmt.Errorf("share /dev/shm: %w", err)
		}

		// mode=/size=/nosuid/etc. are tmpfs-specific and meaningless (or
		// invalid) on a bind mount; rbind is the only option this needs.
		b.Spec.Mounts[i] = specs.Mount{
			Destination: m.Destination,
			Type:        "bind",
			Source:      guestPath,
			Options:     []string{"rbind"},
		}
	}

	return nil
}

// containerSharesIPC reports whether b's spec asks to join a shared IPC
// namespace — the same non-empty-Path test sanitizeNamespaces uses (see its
// doc comment) — regardless of whether sanitizeNamespaces has already run
// and rewritten that Path to the guest's shared namespace: either way, a
// present entry has a non-empty Path if and only if sharing was requested.
func containerSharesIPC(b *bundle.Bundle) bool {
	if b.Spec.Linux == nil {
		return false
	}
	for _, ns := range b.Spec.Linux.Namespaces {
		if ns.Type == specs.IPCNamespace && ns.Path != "" {
			return true
		}
	}
	return false
}

// devShmSize parses a tmpfs "size=" mount option (e.g. "size=65536k", the
// form containerd sends) into a byte count, falling back to
// defaultDevShmSize if opts has none or it can't be parsed. Only the
// suffixes tmpfs itself accepts (k/m/g, case-insensitive) are handled; a
// suffix this doesn't recognize (e.g. a raw percentage) falls back too,
// rather than guessing.
func devShmSize(opts []string) int64 {
	for _, o := range opts {
		v, ok := strings.CutPrefix(o, "size=")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		if len(v) < 2 {
			continue
		}
		n, err := strconv.ParseInt(v[:len(v)-1], 10, 64)
		if err != nil {
			continue
		}
		switch v[len(v)-1] | 0x20 { // lowercase the suffix byte
		case 'k':
			return n * 1024
		case 'm':
			return n * 1024 * 1024
		case 'g':
			return n * 1024 * 1024 * 1024
		}
	}
	return defaultDevShmSize
}
