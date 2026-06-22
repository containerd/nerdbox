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
	"testing"

	"github.com/containerd/containerd/api/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func applyOpts(opts []sandbox.Opt) sandbox.Options {
	var o sandbox.Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func TestBlockMountsProvider(t *testing.T) {
	const id = "cid"

	testcases := []struct {
		name           string
		mounts         []specs.Mount
		wantDisks      []sandbox.Disk
		wantSpecMounts []specs.Mount
		wantVmMounts   []mount.Mount
	}{
		{
			name:           "no mounts",
			mounts:         nil,
			wantDisks:      nil,
			wantSpecMounts: nil,
			wantVmMounts:   nil,
		},
		{
			name: "no ext4 mounts",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantDisks: nil,
			wantSpecMounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantVmMounts: nil,
		},
		{
			name: "single ext4 mount read-write",
			mounts: []specs.Mount{
				{Type: "ext4", Source: "/vol/myvolume.img", Destination: "/data"},
			},
			wantDisks: []sandbox.Disk{
				{BlockID: "disk-97-cid", MountPath: "/vol/myvolume.img", Flags: 0},
			},
			wantSpecMounts: []specs.Mount{
				{Type: "bind", Source: "/run/mnt/sda", Destination: "/data", Options: []string{"rbind"}},
			},
			wantVmMounts: []mount.Mount{
				{Type: "ext4", Source: "/dev/vda", Target: "/run/mnt/sda"},
			},
		},
		{
			name: "single ext4 mount read-only",
			mounts: []specs.Mount{
				{Type: "ext4", Source: "/vol/myvolume.img", Destination: "/data", Options: []string{"ro"}},
			},
			wantDisks: []sandbox.Disk{
				{BlockID: "disk-97-cid", MountPath: "/vol/myvolume.img", Flags: sandbox.DiskFlagReadonly},
			},
			wantSpecMounts: []specs.Mount{
				{Type: "bind", Source: "/run/mnt/sda", Destination: "/data", Options: []string{"rbind", "ro"}},
			},
			wantVmMounts: []mount.Mount{
				{Type: "ext4", Source: "/dev/vda", Target: "/run/mnt/sda", Options: []string{"ro"}},
			},
		},
		{
			name: "multiple ext4 mounts",
			mounts: []specs.Mount{
				{Type: "ext4", Source: "/vol/vol1.img", Destination: "/data"},
				{Type: "ext4", Source: "/vol/vol2.img", Destination: "/logs", Options: []string{"ro"}},
			},
			wantDisks: []sandbox.Disk{
				{BlockID: "disk-97-cid", MountPath: "/vol/vol1.img", Flags: 0},
				{BlockID: "disk-98-cid", MountPath: "/vol/vol2.img", Flags: sandbox.DiskFlagReadonly},
			},
			wantSpecMounts: []specs.Mount{
				{Type: "bind", Source: "/run/mnt/sda", Destination: "/data", Options: []string{"rbind"}},
				{Type: "bind", Source: "/run/mnt/sdb", Destination: "/logs", Options: []string{"rbind", "ro"}},
			},
			wantVmMounts: []mount.Mount{
				{Type: "ext4", Source: "/dev/vda", Target: "/run/mnt/sda"},
				{Type: "ext4", Source: "/dev/vdb", Target: "/run/mnt/sdb", Options: []string{"ro"}},
			},
		},
		{
			name: "mixed mount types",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "ext4", Source: "/vol/myvolume.img", Destination: "/data"},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantDisks: []sandbox.Disk{
				{BlockID: "disk-97-cid", MountPath: "/vol/myvolume.img", Flags: 0},
			},
			wantSpecMounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "bind", Source: "/run/mnt/sda", Destination: "/data", Options: []string{"rbind"}},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantVmMounts: []mount.Mount{
				{Type: "ext4", Source: "/dev/vda", Target: "/run/mnt/sda"},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			b := &bundle.Bundle{
				Spec: specs.Spec{Mounts: tc.mounts},
			}

			da := newDiskAllocator(0)
			bm := &blockMounter{}
			err := bm.FromBundle(context.Background(), b, id, &da)
			assert.NoError(t, err)

			opts := applyOpts(bm.SandboxOpts())
			assert.Equal(t, tc.wantDisks, opts.Disks)
			assert.Equal(t, tc.wantSpecMounts, b.Spec.Mounts)
			assert.Equal(t, tc.wantVmMounts, bm.VmMounts())
		})
	}
}

func TestBindMountsProvider(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a test file
	testfile := filepath.Join(tmpDir, "testfile.txt")
	f, err := os.Create(testfile)
	assert.NoError(t, err)
	f.Close()

	// Create a test directory
	testdirData := filepath.Join(tmpDir, "testdir", "data")
	assert.NoError(t, os.MkdirAll(testdirData, 0755))
	testdirConfig := filepath.Join(tmpDir, "testdir", "config")
	assert.NoError(t, os.MkdirAll(testdirConfig, 0755))

	testcases := []struct {
		name            string
		mounts          []specs.Mount
		wantMounts      []bindMount
		wantSpecSources []string // expected sources in the OCI spec after transformation
		wantVmMounts    []mount.Mount
		wantFS          []sandbox.Filesystem
	}{
		{
			name:            "no mounts",
			mounts:          nil,
			wantMounts:      nil,
			wantSpecSources: nil,
			wantVmMounts:    nil,
		},
		{
			name: "no bind mounts",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantMounts:      nil,
			wantSpecSources: []string{"tmpfs", "proc"},
			wantVmMounts:    nil,
		},
		{
			name: "single bind mount",
			mounts: []specs.Mount{
				{Type: "bind", Source: testdirData, Destination: "/container/data"},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-8c5eaa445dd84f17",
					hostSrc:  testdirData,
					vmTarget: "/run/mnt/bind-8c5eaa445dd84f17",
				},
			},
			wantSpecSources: []string{"/run/mnt/bind-8c5eaa445dd84f17"},
			wantVmMounts: []mount.Mount{
				{Type: "virtiofs", Source: "bind-8c5eaa445dd84f17", Target: "/run/mnt/bind-8c5eaa445dd84f17"},
			},
			wantFS: []sandbox.Filesystem{
				{Tag: "bind-8c5eaa445dd84f17", MountPath: testdirData, Readonly: false},
			},
		},
		{
			name: "single bind mount read-only",
			mounts: []specs.Mount{
				{Type: "bind", Source: testdirData, Destination: "/container/data", Options: []string{"rbind", "ro"}},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-8c5eaa445dd84f17",
					hostSrc:  testdirData,
					vmTarget: "/run/mnt/bind-8c5eaa445dd84f17",
					readOnly: true,
				},
			},
			wantSpecSources: []string{"/run/mnt/bind-8c5eaa445dd84f17"},
			wantVmMounts: []mount.Mount{
				{Type: "virtiofs", Source: "bind-8c5eaa445dd84f17", Target: "/run/mnt/bind-8c5eaa445dd84f17"},
			},
			wantFS: []sandbox.Filesystem{
				{Tag: "bind-8c5eaa445dd84f17", MountPath: testdirData, Readonly: true},
			},
		},
		{
			// A read-only *file* bind mount is backed by sharing its parent
			// directory (virtio-fs operates on directories), so the share itself
			// must stay writable — the parent holds unrelated writable siblings.
			// The file keeps its read-only guarantee via the guest `ro` mount
			// preserved in wantSpecSources. Hence readOnly/Readonly are false.
			name: "single file bind mount read-only",
			mounts: []specs.Mount{
				{Type: "bind", Source: testfile, Destination: "/container/testfile", Options: []string{"bind", "ro"}},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-6dace5108a719565",
					hostSrc:  tmpDir,
					vmTarget: "/run/mnt/bind-6dace5108a719565",
					readOnly: false,
				},
			},
			wantSpecSources: []string{"/run/mnt/bind-6dace5108a719565/testfile.txt"},
			wantVmMounts: []mount.Mount{
				{Type: "virtiofs", Source: "bind-6dace5108a719565", Target: "/run/mnt/bind-6dace5108a719565"},
			},
			wantFS: []sandbox.Filesystem{
				{Tag: "bind-6dace5108a719565", MountPath: tmpDir, Readonly: false},
			},
		},
		{
			name: "multiple bind mounts",
			mounts: []specs.Mount{
				{Type: "bind", Source: testdirData, Destination: "/container/data"},
				{Type: "bind", Source: testdirConfig, Destination: "/container/config", Options: []string{"rbind", "ro"}},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-8c5eaa445dd84f17",
					hostSrc:  testdirData,
					vmTarget: "/run/mnt/bind-8c5eaa445dd84f17",
				},
				{
					tag:      "bind-529984c9ac58b7ec",
					hostSrc:  testdirConfig,
					vmTarget: "/run/mnt/bind-529984c9ac58b7ec",
					readOnly: true,
				},
			},
			wantSpecSources: []string{
				"/run/mnt/bind-8c5eaa445dd84f17",
				"/run/mnt/bind-529984c9ac58b7ec",
			},
			wantVmMounts: []mount.Mount{
				{Type: "virtiofs", Source: "bind-8c5eaa445dd84f17", Target: "/run/mnt/bind-8c5eaa445dd84f17"},
				{Type: "virtiofs", Source: "bind-529984c9ac58b7ec", Target: "/run/mnt/bind-529984c9ac58b7ec"},
			},
			wantFS: []sandbox.Filesystem{
				{Tag: "bind-8c5eaa445dd84f17", MountPath: testdirData, Readonly: false},
				{Tag: "bind-529984c9ac58b7ec", MountPath: testdirConfig, Readonly: true},
			},
		},
		{
			name: "mixed mount types",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "bind", Source: testdirData, Destination: "/container/data"},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-8c5eaa445dd84f17",
					hostSrc:  testdirData,
					vmTarget: "/run/mnt/bind-8c5eaa445dd84f17",
				},
			},
			wantSpecSources: []string{
				"tmpfs",
				"/run/mnt/bind-8c5eaa445dd84f17",
				"proc",
			},
			wantVmMounts: []mount.Mount{
				{Type: "virtiofs", Source: "bind-8c5eaa445dd84f17", Target: "/run/mnt/bind-8c5eaa445dd84f17"},
			},
			wantFS: []sandbox.Filesystem{
				{Tag: "bind-8c5eaa445dd84f17", MountPath: testdirData, Readonly: false},
			},
		},
		{
			name: "single file bind mount",
			mounts: []specs.Mount{
				{Type: "bind", Source: testfile, Destination: "/container/testfile"},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-6dace5108a719565",
					hostSrc:  tmpDir,
					vmTarget: "/run/mnt/bind-6dace5108a719565",
				},
			},
			wantSpecSources: []string{"/run/mnt/bind-6dace5108a719565/testfile.txt"},
			wantVmMounts: []mount.Mount{
				{Type: "virtiofs", Source: "bind-6dace5108a719565", Target: "/run/mnt/bind-6dace5108a719565"},
			},
			wantFS: []sandbox.Filesystem{
				{Tag: "bind-6dace5108a719565", MountPath: tmpDir, Readonly: false},
			},
		},
		{
			// Later `rw` must override an earlier `ro` to match
			// typical mount-option semantics.
			name: "bind mount ro then rw",
			mounts: []specs.Mount{
				{Type: "bind", Source: testdirData, Destination: "/container/data", Options: []string{"rbind", "ro", "rw"}},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-8c5eaa445dd84f17",
					hostSrc:  testdirData,
					vmTarget: "/run/mnt/bind-8c5eaa445dd84f17",
				},
			},
			wantSpecSources: []string{"/run/mnt/bind-8c5eaa445dd84f17"},
			wantVmMounts: []mount.Mount{
				{Type: "virtiofs", Source: "bind-8c5eaa445dd84f17", Target: "/run/mnt/bind-8c5eaa445dd84f17"},
			},
			wantFS: []sandbox.Filesystem{
				{Tag: "bind-8c5eaa445dd84f17", MountPath: testdirData, Readonly: false},
			},
		},
		{
			// Later `ro` must override an earlier `rw`.
			name: "bind mount rw then ro",
			mounts: []specs.Mount{
				{Type: "bind", Source: testdirData, Destination: "/container/data", Options: []string{"rbind", "rw", "ro"}},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-8c5eaa445dd84f17",
					hostSrc:  testdirData,
					vmTarget: "/run/mnt/bind-8c5eaa445dd84f17",
					readOnly: true,
				},
			},
			wantSpecSources: []string{"/run/mnt/bind-8c5eaa445dd84f17"},
			wantVmMounts: []mount.Mount{
				{Type: "virtiofs", Source: "bind-8c5eaa445dd84f17", Target: "/run/mnt/bind-8c5eaa445dd84f17"},
			},
			wantFS: []sandbox.Filesystem{
				{Tag: "bind-8c5eaa445dd84f17", MountPath: testdirData, Readonly: true},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			b := &bundle.Bundle{
				Spec: specs.Spec{
					Mounts: tc.mounts,
				},
			}

			bm := &bindMounter{}
			err := bm.FromBundle(context.Background(), b)
			assert.NoError(t, err)
			assert.Equal(t, tc.wantMounts, bm.mounts)

			// Verify that the spec sources were transformed
			for i, wantSource := range tc.wantSpecSources {
				assert.Equal(t, wantSource, b.Spec.Mounts[i].Source)
			}

			// Verify the VM mounts passed via MountAll RPC
			assert.Equal(t, tc.wantVmMounts, bm.VmMounts())

			// Verify the sandbox.Filesystem entries (and their
			// Readonly flag) flow correctly through SandboxOpts.
			assert.Equal(t, tc.wantFS, applyOpts(bm.SandboxOpts()).Filesystems)
		})
	}
}

// makeErofsLayer creates a sparse zero-filled file of the given size to act
// as a stand-in for an erofs layer image. transformMounts only stat()s these
// files, so their contents do not need to be a valid erofs filesystem.
func makeErofsLayer(t *testing.T, dir, name string, size int64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	return p
}

// TestTransformMountsErofs covers the erofs branches of transformMounts:
//
//   - a single plain erofs mount becomes a raw virtio-block disk;
//   - a single multi-device erofs mount (with device= options) goes through
//     the existing flat-concat VMDK path inline;
//   - 2..gptLayerThreshold plain erofs mounts each get a raw disk;
//   - more than gptLayerThreshold plain erofs mounts are packed into one
//     GPT-partitioned VMDK consuming a single virtio-block letter;
//   - multi-device and plain erofs mounts coexist: the former are inline,
//     the latter contribute to the threshold-driven GPT decision.
func TestTransformMountsErofs(t *testing.T) {
	const id = "cid"

	t.Run("single plain erofs is a raw disk", func(t *testing.T) {
		tmp := t.TempDir()
		bundleDir := t.TempDir()
		layer := makeErofsLayer(t, tmp, "layer0.img", 4*1024*1024)

		ms := []*types.Mount{{
			Type:    "erofs",
			Source:  layer,
			Target:  "/rootfs",
			Options: []string{"ro"},
		}}

		da := newDiskAllocator(0)
		out, opts, err := transformMounts(context.Background(), id, ms, &da, bundleDir)
		require.NoError(t, err)

		require.Len(t, out, 1)
		assert.Equal(t, "erofs", out[0].Type)
		assert.Equal(t, "/dev/vda", out[0].Source)
		assert.Equal(t, "/rootfs", out[0].Target)
		assert.Equal(t, []string{"ro"}, out[0].Options)

		o := applyOpts(opts)
		require.Len(t, o.Disks, 1)
		assert.Equal(t, layer, o.Disks[0].MountPath)
		assert.Equal(t, sandbox.DiskFlagReadonly, o.Disks[0].Flags,
			"single plain erofs uses raw image, no VMDK flag")

		// No VMDK is generated for the single plain path.
		_, statErr := os.Stat(filepath.Join(bundleDir, "merged_fs.vmdk"))
		assert.True(t, os.IsNotExist(statErr))
		_, statErr = os.Stat(filepath.Join(bundleDir, "merged_fs_gpt.vmdk"))
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("multi-device erofs uses flat-concat VMDK inline", func(t *testing.T) {
		tmp := t.TempDir()
		bundleDir := t.TempDir()

		var deviceOpts []string
		top := makeErofsLayer(t, tmp, "layer-top.img", 4*1024*1024)
		for i := 0; i < 2; i++ {
			lp := makeErofsLayer(t, tmp, fmt.Sprintf("layer-%d.img", i), 4*1024*1024)
			deviceOpts = append(deviceOpts, "device="+lp)
		}

		ms := []*types.Mount{{
			Type:    "erofs",
			Source:  top,
			Target:  "/rootfs",
			Options: append([]string{"ro"}, deviceOpts...),
		}}

		da := newDiskAllocator(0)
		out, opts, err := transformMounts(context.Background(), id, ms, &da, bundleDir)
		require.NoError(t, err)

		require.Len(t, out, 1)
		assert.Equal(t, "/dev/vda", out[0].Source,
			"flat-concat VMDK exposes the layers as a single concatenated device")
		assert.Equal(t, []string{"ro"}, out[0].Options,
			"flat-concat path drops device= options because the VMDK already concatenates layers")

		o := applyOpts(opts)
		require.Len(t, o.Disks, 1)
		// VMDK is written into bundleDir with the disk letter in the name.
		// The multi-device mount gets letter 'a' (first allocation).
		expectedVmdk := filepath.Join(bundleDir, "merged_fs_a.vmdk")
		assert.Equal(t, expectedVmdk, o.Disks[0].MountPath)
		assert.Equal(t, sandbox.DiskFlagReadonly|sandbox.DiskFlagVMDK, o.Disks[0].Flags)

		// The flat-concat VMDK was generated in bundleDir; source dir is untouched.
		_, statErr := os.Stat(expectedVmdk)
		assert.NoError(t, statErr)
		_, statErr = os.Stat(filepath.Join(tmp, "merged_fs.vmdk"))
		assert.True(t, os.IsNotExist(statErr), "VMDK must not be written next to sources")
		_, statErr = os.Stat(filepath.Join(bundleDir, "merged_fs_gpt.vmdk"))
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("plain erofs at or below threshold use raw disks", func(t *testing.T) {
		tmp := t.TempDir()
		bundleDir := t.TempDir()

		// gptLayerThreshold separate plain erofs mounts (no device=).
		// At exactly the threshold the GPT path is NOT taken (the trigger
		// is strictly greater-than).
		var ms []*types.Mount
		for i := 0; i < gptLayerThreshold; i++ {
			lp := makeErofsLayer(t, tmp, fmt.Sprintf("l%d.img", i), 4*1024*1024)
			ms = append(ms, &types.Mount{
				Type:    "erofs",
				Source:  lp,
				Target:  fmt.Sprintf("/m/%d", i),
				Options: []string{"ro"},
			})
		}

		da := newDiskAllocator(0)
		out, opts, err := transformMounts(context.Background(), id, ms, &da, bundleDir)
		require.NoError(t, err)

		require.Len(t, out, gptLayerThreshold)
		o := applyOpts(opts)
		require.Len(t, o.Disks, gptLayerThreshold,
			"each plain erofs at or below threshold gets its own virtio-block disk")

		// Disk letters are allocated in deferred order, but candidates are
		// captured in input order, so output mounts get vda..vd<threshold>.
		for i := 0; i < gptLayerThreshold; i++ {
			assert.Equal(t, fmt.Sprintf("/dev/vd%c", 'a'+byte(i)), out[i].Source)
			assert.Equal(t, fmt.Sprintf("/m/%d", i), out[i].Target)
			// Raw disk: VMDK flag must NOT be set.
			assert.Equal(t, sandbox.DiskFlagReadonly, o.Disks[i].Flags)
		}

		// No GPT VMDK at the threshold.
		_, statErr := os.Stat(filepath.Join(bundleDir, "merged_fs_gpt.vmdk"))
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("plain erofs above threshold packed into GPT VMDK", func(t *testing.T) {
		tmp := t.TempDir()
		bundleDir := t.TempDir()

		mountCount := gptLayerThreshold + 2
		var ms []*types.Mount
		for i := 0; i < mountCount; i++ {
			lp := makeErofsLayer(t, tmp, fmt.Sprintf("l%d.img", i), 4*1024*1024)
			ms = append(ms, &types.Mount{
				Type:    "erofs",
				Source:  lp,
				Target:  fmt.Sprintf("/m/%d", i),
				Options: []string{"ro"},
			})
		}

		da := newDiskAllocator(0)
		out, opts, err := transformMounts(context.Background(), id, ms, &da, bundleDir)
		require.NoError(t, err)

		require.Len(t, out, mountCount)
		// Each plain erofs becomes a partition on the single GPT VMDK
		// (which itself is the first - and only - virtio-block disk: /dev/vda).
		for i := 0; i < mountCount; i++ {
			assert.Equal(t, "erofs", out[i].Type)
			assert.Equal(t, fmt.Sprintf("/dev/vda%d", i+1), out[i].Source,
				"plain erofs mount %d must point at GPT partition %d", i, i+1)
			assert.Equal(t, fmt.Sprintf("/m/%d", i), out[i].Target)
			assert.Equal(t, []string{"ro"}, out[i].Options,
				"per-mount options preserved; each partition gets a separate mount call")
		}

		o := applyOpts(opts)
		require.Len(t, o.Disks, 1, "all plain erofs mounts share a single GPT VMDK disk")
		// VMDK is written into bundleDir, not next to the source layers.
		expectedVmdk := filepath.Join(bundleDir, "merged_fs_gpt.vmdk")
		assert.Equal(t, expectedVmdk, o.Disks[0].MountPath)
		assert.Equal(t, sandbox.DiskFlagReadonly|sandbox.DiskFlagVMDK, o.Disks[0].Flags)

		// GPT VMDK and its auxiliary blobs were generated in bundleDir;
		// source dir is untouched.
		_, statErr := os.Stat(expectedVmdk)
		assert.NoError(t, statErr)
		_, statErr = os.Stat(filepath.Join(bundleDir, "merged_fs_gpt_header.bin"))
		assert.NoError(t, statErr)
		_, statErr = os.Stat(filepath.Join(tmp, "merged_fs_gpt.vmdk"))
		assert.True(t, os.IsNotExist(statErr), "VMDK must not be written next to sources")
		_, statErr = os.Stat(filepath.Join(bundleDir, "merged_fs.vmdk"))
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("multi-device alongside many plain erofs mounts", func(t *testing.T) {
		tmp := t.TempDir()
		bundleDir := t.TempDir()

		// One multi-device erofs (handled inline) plus enough plain erofs
		// mounts to trigger the GPT path.
		mdTop := makeErofsLayer(t, tmp, "md-top.img", 4*1024*1024)
		mdLow := makeErofsLayer(t, tmp, "md-low.img", 4*1024*1024)
		multiDevice := &types.Mount{
			Type:    "erofs",
			Source:  mdTop,
			Target:  "/multi",
			Options: []string{"ro", "device=" + mdLow},
		}

		mountCount := gptLayerThreshold + 1
		var plainMounts []*types.Mount
		for i := 0; i < mountCount; i++ {
			lp := makeErofsLayer(t, tmp, fmt.Sprintf("p%d.img", i), 4*1024*1024)
			plainMounts = append(plainMounts, &types.Mount{
				Type:    "erofs",
				Source:  lp,
				Target:  fmt.Sprintf("/p/%d", i),
				Options: []string{"ro"},
			})
		}

		// Interleave: a couple of plain mounts, the multi-device mount,
		// then the rest of the plain mounts. The multi-device mount's
		// disk letter must come from inline allocation; plain mounts'
		// letter comes from the deferred GPT pass.
		ms := []*types.Mount{}
		ms = append(ms, plainMounts[0:2]...)
		ms = append(ms, multiDevice)
		ms = append(ms, plainMounts[2:]...)

		da := newDiskAllocator(0)
		out, opts, err := transformMounts(context.Background(), id, ms, &da, bundleDir)
		require.NoError(t, err)

		require.Len(t, out, len(ms))
		o := applyOpts(opts)
		require.Len(t, o.Disks, 2, "one multi-device VMDK plus one GPT VMDK = 2 disks")

		// addDisks order: multi-device (allocated inline in pass 1) gets
		// letter 'a'; GPT VMDK (allocated in pass 2) gets letter 'b'.
		// Both are written into bundleDir; multi-device uses a letter-keyed name.
		assert.Equal(t, filepath.Join(bundleDir, "merged_fs_a.vmdk"), o.Disks[0].MountPath)
		assert.Equal(t, sandbox.DiskFlagReadonly|sandbox.DiskFlagVMDK, o.Disks[0].Flags)
		assert.Equal(t, filepath.Join(bundleDir, "merged_fs_gpt.vmdk"), o.Disks[1].MountPath)
		assert.Equal(t, sandbox.DiskFlagReadonly|sandbox.DiskFlagVMDK, o.Disks[1].Flags)

		// Output mount order matches input order:
		//   ms[0] = plain mounts[0] -> /dev/vdb1
		//   ms[1] = plain mounts[1] -> /dev/vdb2
		//   ms[2] = multi-device   -> /dev/vda
		//   ms[3] = plain mounts[2] -> /dev/vdb3
		//   ...
		assert.Equal(t, "/dev/vdb1", out[0].Source)
		assert.Equal(t, "/dev/vdb2", out[1].Source)
		assert.Equal(t, "/dev/vda", out[2].Source)
		for i := 0; i < mountCount-2; i++ {
			assert.Equal(t, fmt.Sprintf("/dev/vdb%d", i+3), out[3+i].Source,
				"plain mount %d after multi-device must keep its sequential partition", i)
		}
	})

	t.Run("GPT VMDK is cached on subsequent calls", func(t *testing.T) {
		tmp := t.TempDir()
		bundleDir := t.TempDir()

		mountCount := gptLayerThreshold + 1
		var ms []*types.Mount
		for i := 0; i < mountCount; i++ {
			lp := makeErofsLayer(t, tmp, fmt.Sprintf("l%d.img", i), 4*1024*1024)
			ms = append(ms, &types.Mount{
				Type:   "erofs",
				Source: lp,
				Target: fmt.Sprintf("/m/%d", i),
			})
		}

		da := newDiskAllocator(0)
		_, _, err := transformMounts(context.Background(), id, ms, &da, bundleDir)
		require.NoError(t, err)

		// VMDK must not be regenerated on a second call with the same inputs.
		gptPath := filepath.Join(bundleDir, "merged_fs_gpt.vmdk")
		fi1, err := os.Stat(gptPath)
		require.NoError(t, err)

		da2 := newDiskAllocator(0)
		_, _, err = transformMounts(context.Background(), id, ms, &da2, bundleDir)
		require.NoError(t, err)

		fi2, err := os.Stat(gptPath)
		require.NoError(t, err)
		assert.Equal(t, fi1.ModTime(), fi2.ModTime(),
			"VMDK descriptor must not be regenerated when it already exists")
	})
}
