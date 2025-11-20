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
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/nerdbox/internal/erofs"
	"github.com/containerd/nerdbox/internal/vm"
)

type diskOptions struct {
	name     string
	source   string
	readOnly bool
	vmdk     bool
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

	log.G(ctx).Trace("transformMounts", ms)
	for _, m := range ms {
		switch m.Type {
		case "erofs":

			disk := fmt.Sprintf("disk-%d-%s", disks, id)
			// virtiofs implementation has a limit of 36 characters for the tag
			if len(disk) > 36 {
				disk = disk[:36]
			}

			var Options []string

			devices := []string{m.Source}
			for _, o := range m.Options {
				if d, f := strings.CutPrefix(o, "device="); f {
					devices = append(devices, d)
					continue
				}
				Options = append(Options, o)
			}

			if len(devices) > 1 {
				// generate VMDK desc for the EROFS flattened fs if it does not exist
				mergedfsPath := filepath.Dir(m.Source) + "/merged_fs.vmdk"
				if _, err := os.Stat(mergedfsPath); err != nil {
					if !os.IsNotExist(err) {
						log.G(ctx).Warnf("failed to stat %v: %v", mergedfsPath, err)
						return nil, errdefs.ErrNotImplemented
					}
					err = erofs.DumpVMDKDescriptorToFile(mergedfsPath, 0xfffffffe, devices)
					if err != nil {
						log.G(ctx).Warnf("failed to generate %v: %v", mergedfsPath, err)
						return nil, errdefs.ErrNotImplemented
					}
				}
				addDisks = append(addDisks, diskOptions{
					name:     disk,
					source:   mergedfsPath,
					readOnly: true,
					vmdk:     true,
				})
			} else {
				addDisks = append(addDisks, diskOptions{
					name:     disk,
					source:   m.Source,
					readOnly: true,
					vmdk:     false,
				})
			}
			am = append(am, &types.Mount{
				Type:    "erofs",
				Source:  fmt.Sprintf("/dev/vd%c", disks),
				Target:  m.Target,
				Options: filterOptions(Options),
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
				vmdk:     false,
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
		if do.vmdk {
			opts = append(opts, vm.WithVmdk())
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
