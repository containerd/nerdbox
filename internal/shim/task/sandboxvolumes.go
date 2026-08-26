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
	"os"

	"github.com/containerd/log"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// sandboxVolumeMounter is a bundle.Transformer for sandbox member
// containers that rewrites OCI "bind" mounts to reference the sandbox's
// shared filesystem tree instead of a new per-mount virtiofs share.
//
// This is the sandboxed-path counterpart to bindMounter (mount.go), which
// is used by the legacy/plain-container path: that path boots a fresh VM
// per container and can add a new virtiofs share before boot
// (sandbox.WithFS), so giving every bind mount its own virtiofs tag works
// fine there. A sandbox member container is created against an
// already-running VM, and virtio-fs shares cannot be hot-added after
// boot — asking the guest to mount a tag that was never wired up on the
// host/VMM side fails immediately (EINVAL). So instead, each bind mount's
// host source is itself bind-mounted (on the host, by
// sandbox.SharedFS.ShareVolume) into the sandbox's shared directory tree,
// which is already exposed to the guest via one persistent, pre-boot
// virtiofs share — the guest sees the content with no new device and no
// extra guest-side mount step at all.
type sandboxVolumeMounter struct {
	fs          *sandbox.SharedFS
	containerID string
	n           int // next volume index to assign
}

// FromBundle rewrites each "bind" mount's Source in the spec to the guest
// path where sandbox.SharedFS.ShareVolume exposes it. Must run after the
// bundle's rootfs mounts are known to fs (order relative to ShareRootfs
// does not matter: volumes live under a separate subtree), but before the
// spec is sent to the guest.
func (vm *sandboxVolumeMounter) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	for i, m := range b.Spec.Mounts {
		if m.Type != "bind" {
			continue
		}

		log.G(ctx).WithField("mount", m).Debug("sharing bind mount volume via the sandbox virtiofs tree")

		fi, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("failed to stat bind mount source %s: %w", m.Source, err)
		}

		// Only Source changes here — Options (ro/rw, recursive or not,
		// propagation) are left exactly as the spec requested, so crun's
		// own bind mount from the returned guest path into the container
		// is what actually enforces them. See ShareVolume's doc comment
		// for why duplicating read-only enforcement at this layer would
		// be actively wrong, not just redundant.
		guestPath, err := vm.fs.ShareVolume(ctx, vm.containerID, vm.n, m.Source, fi.IsDir())
		if err != nil {
			return fmt.Errorf("share volume mount %s: %w", m.Source, err)
		}
		vm.n++

		b.Spec.Mounts[i].Source = guestPath
	}

	return nil
}
