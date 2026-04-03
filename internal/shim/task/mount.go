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
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/nerdbox/internal/erofs"
	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// diskAllocator assigns sequential virtio disk letters (vda, vdb, …).
// A single instance is shared across rootfs and volume disk allocation so
// that all disks within a container get unique, collision-free letters.
type diskAllocator struct{ next byte }

func newDiskAllocator() diskAllocator { return diskAllocator{next: 'a'} }

func (d *diskAllocator) Next() byte { c := d.next; d.next++; return c }

// count returns the total number of disks allocated so far.
func (d *diskAllocator) count() int { return int(d.next - 'a') }

type diskOptions struct {
	name     string
	source   string
	readOnly bool
	vmdk     bool
}

// transformMounts does not perform any local mounts but transforms
// the mounts to be used inside the VM via virtio
func transformMounts(ctx context.Context, id string, ms []*types.Mount, da *diskAllocator) ([]*types.Mount, []sandbox.Opt, error) {
	var (
		addDisks []diskOptions
		am       []*types.Mount
		sbOpts   []sandbox.Opt
		err      error
	)

	log.G(ctx).Trace("transformMounts", ms)
	for _, m := range ms {
		switch m.Type {
		case "erofs":
			letter := da.Next()
			disk := fmt.Sprintf("disk-%d-%s", letter, id)
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
						return nil, nil, errdefs.ErrNotImplemented
					}
					err = erofs.DumpVMDKDescriptorToFile(mergedfsPath, 0xfffffffe, devices)
					if err != nil {
						log.G(ctx).Warnf("failed to generate %v: %v", mergedfsPath, err)
						return nil, nil, errdefs.ErrNotImplemented
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
				Source:  fmt.Sprintf("/dev/vd%c", letter),
				Target:  m.Target,
				Options: filterOptions(Options),
			})

		case "ext4":
			letter := da.Next()
			disk := fmt.Sprintf("disk-%d-%s", letter, id)
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
				Source:  fmt.Sprintf("/dev/vd%c", letter),
				Target:  m.Target,
				Options: filterOptions(m.Options),
			})
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
					return nil, nil, fmt.Errorf("cannot use virtiofs for upper dir in overlay: %w", errdefs.ErrNotImplemented)
				}
			} else {
				log.G(ctx).WithField("options", m.Options).Warnf("overlayfs missing workdir or upperdir")
			}

			am = append(am, m)
		default:
			am = append(am, m)
		}
	}

	for _, do := range addDisks {
		var flags sandbox.DiskFlags
		if do.readOnly {
			flags |= sandbox.DiskFlagReadonly
		}
		if do.vmdk {
			flags |= sandbox.DiskFlagVMDK
		}

		sbOpts = append(sbOpts, sandbox.WithDisk(do.name, do.source, flags))
	}

	return am, sbOpts, err
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
			// Use path.Join (not filepath.Join) because this path is used
			// inside the Linux VM where forward slashes are required.
			specSrc = path.Join(vmTarget, filepath.Base(m.Source))
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

func (bm *bindMounter) SandboxOpts() []sandbox.Opt {
	var opts []sandbox.Opt
	for _, m := range bm.mounts {
		opts = append(opts, sandbox.WithFS(m.tag, m.hostSrc, false))
	}
	return opts
}

func (bm *bindMounter) VmMounts() []mount.Mount {
	var mounts []mount.Mount
	for _, m := range bm.mounts {
		mounts = append(mounts, mount.Mount{
			Type:   "virtiofs",
			Source: m.tag,
			Target: m.vmTarget,
		})
	}
	return mounts
}

// blockMounter transforms ext4 volume mounts in the OCI spec into
// virtio-block devices. It must be called after setupMounts so that
// rootfs disks are allocated first and volume disks follow sequentially.
type blockMounter struct {
	opts     []sandbox.Opt
	vmMounts []mount.Mount
}

// FromBundle iterates the OCI spec mounts and for each ext4 mount:
//   - allocates a virtio-block disk letter via the shared diskAllocator
//   - rewrites the spec source to /mnt/sdX (where vminitd will mount it)
//   - records a sandbox.WithDisk opt for the host image path
//   - records a -blockmount init arg so vminitd mounts the device at startup
func (bm *blockMounter) FromBundle(ctx context.Context, b *bundle.Bundle, id string, da *diskAllocator) error {
	for i, m := range b.Spec.Mounts {
		if m.Type != "ext4" {
			continue
		}

		log.G(ctx).WithField("mount", m).Debug("transforming ext4 volume mount to virtio-block device")

		letter := da.Next()
		diskName := fmt.Sprintf("disk-%d-%s", letter, id)
		if len(diskName) > 36 {
			diskName = diskName[:36]
		}

		device := fmt.Sprintf("/dev/vd%c", letter)
		vmTarget := fmt.Sprintf("/mnt/sd%c", letter)

		hostSrc := b.Spec.Mounts[i].Source
		b.Spec.Mounts[i].Type = "bind"
		b.Spec.Mounts[i].Source = vmTarget

		readonly := false
		var flags sandbox.DiskFlags
		for _, opt := range m.Options {
			if opt == "ro" {
				flags |= sandbox.DiskFlagReadonly
				readonly = true
				break
			}
		}
		bindOpts := []string{"rbind"}
		if readonly {
			bindOpts = append(bindOpts, "ro")
		}
		b.Spec.Mounts[i].Options = bindOpts

		bm.opts = append(bm.opts, sandbox.WithDisk(diskName, hostSrc, flags))

		vmMount := mount.Mount{
			Type:    "ext4",
			Source:  device,
			Target:  vmTarget,
			Options: m.Options,
		}
		bm.vmMounts = append(bm.vmMounts, vmMount)
	}
	return nil
}

func (bm *blockMounter) SandboxOpts() []sandbox.Opt {
	return bm.opts
}

func (bm *blockMounter) VmMounts() []mount.Mount {
	return bm.vmMounts
}
