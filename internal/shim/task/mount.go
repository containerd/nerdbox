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
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/log"

	"github.com/dmcgowan/nerdbox/internal/vm"
)

// transformMounts does not perform any local mounts but transforms
// the mounts to be used inside the VM via virtio
func transformMounts(ctx context.Context, vmi vm.Instance, id string, ms []*types.Mount) ([]*types.Mount, error) {
	var (
		disks byte = 'a'
		am    []*types.Mount
	)

	for _, m := range ms {
		switch m.Type {
		case "erofs":
			disk := fmt.Sprintf("disk-%d-%s", disks, id)
			// virtiofs implementation has a limit of 36 characters for the tag
			if len(disk) > 36 {
				disk = disk[:36]
			}
			if err := vmi.AddDisk(ctx, disk, m.Source, vm.WithReadOnly()); err != nil {
				return nil, err
			}
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
			if err := vmi.AddDisk(ctx, disk, m.Source); err != nil {
				return nil, err
			}
			am = append(am, &types.Mount{
				Type:    "ext4",
				Source:  fmt.Sprintf("/dev/vd%c", disks),
				Target:  m.Target,
				Options: filterOptions(m.Options),
			})
			disks++
		case "format/overlay":
			var (
				wdi int = -1
				udi int = -1
			)
			for i, opt := range m.Options {
				if strings.Contains(opt, "{{") {
					continue
				}
				if strings.HasPrefix(opt, "upperdir=") {
					udi = i
				} else if strings.HasPrefix(opt, "workdir=") {
					wdi = i
				}
				// TODO: Handle virtio for lowers?
			}
			if wdi > -1 && udi > -1 {
				udir, uname := filepath.Split(m.Options[udi][len("upperdir="):])

				wdir, wname := filepath.Split(m.Options[wdi][len("workdir="):])
				if udir == wdir {
					tag := fmt.Sprintf("overlayfs-upper-%s", id)
					// virtiofs implementation has a limit of 36 characters for the tag
					if len(tag) > 36 {
						tag = tag[:36]
					}
					if err := vmi.AddFS(ctx, tag, udir); err != nil {
						return nil, err
					}
					m.Options[udi] = fmt.Sprintf("upperdir={{ mount %d }}/%s", len(am), uname)
					m.Options[wdi] = fmt.Sprintf("workdir={{ mount %d }}/%s", len(am), wname)
					am = append(am, &types.Mount{
						Type:   "virtiofs",
						Source: tag,
					})
					log.G(ctx).WithFields(log.Fields{
						"workdir":  m.Options[wdi],
						"upperdir": m.Options[udi],
					}).Warnf("transformed upper and work")
				} else {
					log.G(ctx).WithFields(log.Fields{
						"workdir":  m.Options[wdi],
						"upperdir": m.Options[udi],
					}).Warnf("overlayfs workdir and upperdir should be in the same directory")
				}
			} else {
				log.G(ctx).WithField("options", m.Options).Warnf("overlayfs missing workdir or upperdir")
			}

			am = append(am, m)
		default:
			am = append(am, m)
		}
	}

	return am, nil
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
