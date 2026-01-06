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
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/vm"
)

type diskOptions struct {
	name     string
	source   string
	readOnly bool
}

// transformMounts does not perform any local mounts but transforms
// the mounts to be used inside the VM via virtio
func transformMounts(ctx context.Context, vmi vm.Instance, id string, ms []*types.Mount) ([]*types.Mount, error) {
	var (
		disks    byte = 'a'
		addDisks []diskOptions
		am       []*types.Mount
		err      error
	)

	for _, m := range ms {
		switch m.Type {
		case "erofs":
			disk := fmt.Sprintf("disk-%d-%s", disks, id)
			// virtiofs implementation has a limit of 36 characters for the tag
			if len(disk) > 36 {
				disk = disk[:36]
			}
			addDisks = append(addDisks, diskOptions{
				name:     disk,
				source:   m.Source,
				readOnly: true,
			})
			am = append(am, &types.Mount{
				Type:    "erofs",
				Source:  fmt.Sprintf("/dev/vd%c", disks),
				Target:  m.Target,
				Options: filterOptions(m.Options),
			})
			disks++
		case "ext4":
			disk := fmt.Sprintf("disk-%d-%s", disks, id)
			// virtiofs implementation has a limit of 36 characters for the tag
			if len(disk) > 36 {
				disk = disk[:36]
			}
			// TODO: Check read only option
			addDisks = append(addDisks, diskOptions{
				name:     disk,
				source:   m.Source,
				readOnly: false,
			})
			am = append(am, &types.Mount{
				Type:    "ext4",
				Source:  fmt.Sprintf("/dev/vd%c", disks),
				Target:  m.Target,
				Options: filterOptions(m.Options),
			})
			disks++
		case "overlay", "format/overlay", "format/mkdir/overlay":
			var (
				wdi = -1
				udi = -1
			)
			for i, opt := range m.Options {
				if strings.HasPrefix(opt, "upperdir=") {
					udi = i
				} else if strings.HasPrefix(opt, "workdir=") {
					wdi = i
				}
				// TODO: Handle virtio for lowers?
			}
			if wdi > -1 && udi > -1 {
				//
				// If any upperdir or workdir isn't transformed, they both
				// should fall back to virtiofs passthroughfs.  But...
				//
				if !strings.Contains(m.Options[wdi], "{{") ||
					!strings.Contains(m.Options[udi], "{{") {
					// Having the upper as virtiofs may return invalid argument, avoid
					// transforming and attempt to perform the mounts on the host if
					// supported.
					return nil, fmt.Errorf("cannot use virtiofs for upper dir in overlay: %w", errdefs.ErrNotImplemented)
				}
			} else {
				log.G(ctx).WithField("options", m.Options).Warnf("overlayfs missing workdir or upperdir")
			}

			am = append(am, m)
		default:
			am = append(am, m)
		}
	}

	if len(addDisks) > 10 {
		return nil, fmt.Errorf("exceeded maximum virtio disk count: %d > 10: %w", len(addDisks), errdefs.ErrNotImplemented)
	}

	for _, do := range addDisks {
		var opts []vm.MountOpt
		if do.readOnly {
			opts = append(opts, vm.WithReadOnly())
		}
		if err := vmi.AddDisk(ctx, do.name, do.source, opts...); err != nil {
			return nil, err
		}
	}

	return am, err
}

func filterOptions(options []string) []string {
	var filtered []string
	for _, o := range options {
		switch o {
		case "loop":
		default:
			filtered = append(filtered, o)
		}
	}
	return filtered
}

type bindMounter struct {
	mounts []bindMount
}

type bindMount struct {
	tag      string
	hostSrc  string
	vmTarget string
}

func (bm *bindMounter) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	for i, m := range b.Spec.Mounts {
		if m.Type != "bind" {
			continue
		}

		log.G(ctx).WithField("mount", m).Debug("transforming bind mount into a virtiofs mount")

		fi, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("failed to stat bind mount source %s: %w", m.Source, err)
		}

		hash := sha256.Sum256([]byte(m.Destination))
		tag := fmt.Sprintf("bind-%x", hash[:8])
		vmTarget := "/mnt/" + tag

		// For files, share the parent directory via virtiofs since virtiofs
		// operates on directories. The spec source points to the file within
		// the mounted directory.
		hostSrc := m.Source
		specSrc := vmTarget
		if !fi.IsDir() {
			hostSrc = filepath.Dir(m.Source)
			specSrc = filepath.Join(vmTarget, filepath.Base(m.Source))
		}

		transformed := bindMount{
			tag:      tag,
			hostSrc:  hostSrc,
			vmTarget: vmTarget,
		}

		bm.mounts = append(bm.mounts, transformed)
		b.Spec.Mounts[i].Source = specSrc
	}

	return nil
}

func (bm *bindMounter) SetupVM(ctx context.Context, vmi vm.Instance) error {
	for _, m := range bm.mounts {
		if err := vmi.AddFS(ctx, m.tag, m.hostSrc); err != nil {
			return err
		}
	}
	return nil
}

func (bm *bindMounter) InitArgs() []string {
	args := make([]string, 0, len(bm.mounts))
	for _, m := range bm.mounts {
		args = append(args, "-mount="+m.tag+":"+m.vmTarget)
	}
	return args
}
