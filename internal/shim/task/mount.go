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

// gptLayerThreshold is the number of plain (non-multi-device) erofs mounts
// above which the shim packs them into a single GPT-partitioned VMDK rather
// than allocating one virtio-block device per mount. Multi-device erofs
// mounts (those carrying device= options) are not affected: they continue
// to use the existing flat-concat VMDK path inline.
//
// Packing into a GPT VMDK reduces virtio-block consumption (vda–vdz = 26
// devices total, some of which are reserved by the VM implementation) and
// lets the shim handle deep stacks of independent erofs mounts without
// coordinating layer offsets in the snapshotter.
const gptLayerThreshold = 8

// diskAllocator assigns sequential virtio disk letters starting after any
// disks reserved by the VM implementation (see vm.Manager.ReservedDisks).
// A single instance is shared across rootfs and volume disk allocation so
// that all disks within a container get unique, collision-free letters.
type diskAllocator struct {
	start byte // first letter available for container disks
	next  byte // next letter to assign
}

// newDiskAllocator returns a diskAllocator whose first letter is
// 'a'+reserved, skipping the virtio-block devices the VM implementation
// pre-attaches before any container disks.
func newDiskAllocator(reserved int) diskAllocator {
	first := byte('a') + byte(reserved)
	return diskAllocator{start: first, next: first}
}

func (d *diskAllocator) Next() byte { c := d.next; d.next++; return c }

// count returns the number of container disks allocated so far.
func (d *diskAllocator) count() int { return int(d.next - d.start) }

type diskOptions struct {
	name     string
	source   string
	readOnly bool
	vmdk     bool
}

// erofsCandidate describes a plain erofs mount that has been deferred from
// the first pass of transformMounts. After all mounts are classified, the
// number of candidates determines whether they are each turned into raw
// virtio-block disks or packed into a single GPT-partitioned VMDK.
type erofsCandidate struct {
	outIdx  int      // index in `am` reserved for this mount
	source  string   // host path of the erofs file
	target  string   // VM target path
	options []string // remaining options (no device= entries)
}

// transformMounts does not perform any local mounts but transforms
// the mounts to be used inside the VM via virtio.
//
// erofs mounts that carry device= options are processed inline using the
// existing flat-concat VMDK path. Plain erofs mounts (no device= options)
// are deferred and resolved after the first pass: when more than
// gptLayerThreshold are present, they are packed into a single GPT-
// partitioned VMDK with one partition per mount; otherwise each becomes
// its own raw virtio-block device.
//
// All generated VMDK descriptor files and auxiliary blobs are written into
// bundleDir, which is owned by the shim and torn down with the container.
// The shim must not write into the source image directories.
func transformMounts(ctx context.Context, id string, ms []*types.Mount, da *diskAllocator, bundleDir string) ([]*types.Mount, []sandbox.Opt, error) {
	var (
		addDisks  []diskOptions
		am        []*types.Mount
		sbOpts    []sandbox.Opt
		erofsList []erofsCandidate
	)

	log.G(ctx).Trace("transformMounts", ms)
	for _, m := range ms {
		switch m.Type {
		case "erofs":
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
				// Multi-device erofs: existing flat-concat VMDK path,
				// applied inline so it is independent of GPT packing.
				// Write the descriptor into bundleDir (owned by the shim)
				// and regenerate unconditionally so it always reflects the
				// current device list; the shim must not mutate source dirs.
				letter := da.Next()
				disk := fmt.Sprintf("disk-%d-%s", letter, id)
				if len(disk) > 36 {
					disk = disk[:36]
				}
				// Use the disk letter in the filename so that multiple
				// multi-device erofs mounts within the same bundle each get
				// a distinct descriptor and don't overwrite each other.
				mergedfsPath := filepath.Join(bundleDir, fmt.Sprintf("merged_fs_%c.vmdk", letter))
				if err := erofs.DumpVMDKDescriptorToFile(mergedfsPath, 0xfffffffe, devices); err != nil {
					log.G(ctx).WithError(err).WithField("path", mergedfsPath).Warn("failed to generate erofs vmdk descriptor")
					return nil, nil, fmt.Errorf("erofs vmdk: %w", errdefs.ErrNotImplemented)
				}
				addDisks = append(addDisks, diskOptions{
					name:     disk,
					source:   mergedfsPath,
					readOnly: true,
					vmdk:     true,
				})
				am = append(am, &types.Mount{
					Type:    "erofs",
					Source:  fmt.Sprintf("/dev/vd%c", letter),
					Target:  m.Target,
					Options: filterOptions(Options),
				})
				continue
			}

			// Plain erofs: defer and reserve a slot in `am` so the output
			// preserves the input mount ordering.
			am = append(am, nil)
			erofsList = append(erofsList, erofsCandidate{
				outIdx:  len(am) - 1,
				source:  m.Source,
				target:  m.Target,
				options: Options,
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
				log.G(ctx).WithField("options", m.Options).Warn("overlayfs missing workdir or upperdir")
			}

			am = append(am, m)
		default:
			am = append(am, m)
		}
	}

	if err := finalizeErofsCandidates(ctx, id, da, bundleDir, erofsList, am, &addDisks); err != nil {
		return nil, nil, err
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

	return am, sbOpts, nil
}

// finalizeErofsCandidates resolves the deferred plain erofs mounts. When
// the candidate count exceeds gptLayerThreshold, all candidates are packed
// into a single GPT-partitioned VMDK consuming one virtio-block letter.
// Below the threshold, each candidate becomes its own raw virtio-block
// device, matching the long-standing single-mount-per-disk behavior.
//
// The GPT VMDK descriptor and its auxiliary blobs are written into bundleDir,
// which is owned by the shim and torn down with the container.
//
// The reserved slots in `am` (set during the first pass of transformMounts)
// are filled in with the resulting mounts; new disks are appended to
// addDisks. The order of da.Next() calls inside this function is contiguous
// with whatever the first pass already consumed, preserving the invariant
// that the Nth virtio-block letter corresponds to the Nth disk in addDisks.
func finalizeErofsCandidates(ctx context.Context, id string, da *diskAllocator, bundleDir string, candidates []erofsCandidate, am []*types.Mount, addDisks *[]diskOptions) error {
	if len(candidates) == 0 {
		return nil
	}

	if len(candidates) > gptLayerThreshold {
		sources := make([]string, len(candidates))
		for i, c := range candidates {
			sources[i] = c.source
		}
		// Write the GPT VMDK and its auxiliary blobs into bundleDir so the
		// shim does not mutate the source image directories.  Cache by
		// stat-check: setupMounts may be called more than once per bundle
		// (e.g. on restore) but the layer set for a given bundle is fixed.
		gptPath := filepath.Join(bundleDir, "merged_fs_gpt.vmdk")
		if _, err := os.Stat(gptPath); err != nil {
			if !os.IsNotExist(err) {
				log.G(ctx).WithError(err).WithField("path", gptPath).Warn("failed to stat erofs gpt vmdk descriptor")
				return fmt.Errorf("erofs gpt vmdk: %w", errdefs.ErrNotImplemented)
			}
			if err := erofs.DumpGPTVMDKDescriptorToFile(gptPath, 0xfffffffe, sources); err != nil {
				log.G(ctx).WithError(err).WithField("path", gptPath).Warn("failed to generate erofs gpt vmdk descriptor")
				return fmt.Errorf("erofs gpt vmdk: %w", errdefs.ErrNotImplemented)
			}
		}
		letter := da.Next()
		disk := fmt.Sprintf("disk-%d-%s", letter, id)
		if len(disk) > 36 {
			disk = disk[:36]
		}
		*addDisks = append(*addDisks, diskOptions{
			name:     disk,
			source:   gptPath,
			readOnly: true,
			vmdk:     true,
		})
		for i, c := range candidates {
			am[c.outIdx] = &types.Mount{
				Type:    "erofs",
				Source:  fmt.Sprintf("/dev/vd%c%d", letter, i+1),
				Target:  c.target,
				Options: filterOptions(c.options),
			}
		}
		return nil
	}

	// Below the threshold: one raw virtio-block device per candidate.
	for _, c := range candidates {
		letter := da.Next()
		disk := fmt.Sprintf("disk-%d-%s", letter, id)
		if len(disk) > 36 {
			disk = disk[:36]
		}
		*addDisks = append(*addDisks, diskOptions{
			name:     disk,
			source:   c.source,
			readOnly: true,
			vmdk:     false,
		})
		am[c.outIdx] = &types.Mount{
			Type:    "erofs",
			Source:  fmt.Sprintf("/dev/vd%c", letter),
			Target:  c.target,
			Options: filterOptions(c.options),
		}
	}
	return nil
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
	readOnly bool
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
		vmTarget := "/run/mnt/" + tag

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

		// Honor a read-only request from the OCI spec by marking the
		// virtiofs share read-only at the host edge. The guest `ro`
		// mount option preserved in the OCI spec acts as defense in
		// depth. Match typical mount-option semantics: scan all options
		// without short-circuiting so that later `rw` overrides an
		// earlier `ro` (and vice-versa).
		readOnly := false
		for _, opt := range m.Options {
			switch opt {
			case "ro":
				readOnly = true
			case "rw":
				readOnly = false
			}
		}

		transformed := bindMount{
			tag:      tag,
			hostSrc:  hostSrc,
			vmTarget: vmTarget,
			readOnly: readOnly,
		}

		bm.mounts = append(bm.mounts, transformed)
		b.Spec.Mounts[i].Source = specSrc
	}

	return nil
}

func (bm *bindMounter) SandboxOpts() []sandbox.Opt {
	var opts []sandbox.Opt
	for _, m := range bm.mounts {
		opts = append(opts, sandbox.WithFS(m.tag, m.hostSrc, m.readOnly))
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
//   - rewrites the spec source to /run/mnt/sdX (where vminitd will mount it)
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
		vmTarget := fmt.Sprintf("/run/mnt/sd%c", letter)

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
