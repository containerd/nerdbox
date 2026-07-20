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
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/api/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func TestSocketForwardsProviderFromBundle(t *testing.T) {
	ctx := context.Background()

	testcases := []struct {
		name        string
		cid         string
		mounts      []specs.Mount
		wantErr     string
		wantMounts  []specs.Mount
		wantEntries []socketForwardEntry
	}{
		{
			name: "empty source",
			cid:  "c1",
			mounts: []specs.Mount{
				{Type: "uds", Source: "", Destination: "/run/docker.sock"},
			},
			wantErr: "source (host path) is required",
		},
		{
			name: "empty destination",
			cid:  "c1",
			mounts: []specs.Mount{
				{Type: "uds", Source: "/var/run/docker.sock", Destination: ""},
			},
			wantErr: "destination (container path) is required",
		},
		{
			name: "non-empty options",
			cid:  "c1",
			mounts: []specs.Mount{
				{
					Type:        "uds",
					Source:      "/var/run/docker.sock",
					Destination: "/run/docker.sock",
					Options:     []string{"first", "second"},
				},
			},
			wantErr: `unknown option "first, second"`,
		},
		{
			name: "uds mount rewritten to bind",
			cid:  "container-abc",
			mounts: []specs.Mount{
				{
					Type:        "uds",
					Source:      "/var/run/foo.sock",
					Destination: "/var/run/bar.sock",
				},
				{
					Type:        "uds",
					Source:      "/var/run/abc.sock",
					Destination: "/var/run/def.sock",
				},
			},
			wantMounts: []specs.Mount{
				{
					Type:        "bind",
					Source:      "/run/socketfwd/954a6df32e91bb55e6fcd9df9f90728e56b4f87aa92b22fe3b63df33f18a3188.sock",
					Destination: "/var/run/bar.sock",
					Options:     []string{"bind"},
				},
				{
					Type:        "bind",
					Source:      "/run/socketfwd/79a1c3c374d3573c0e7f1e7ca567d3531b3aefd3c202b8811df64e77fdaeab0c.sock",
					Destination: "/var/run/def.sock",
					Options:     []string{"bind"},
				},
			},
			wantEntries: []socketForwardEntry{
				{
					id:            "954a6df32e91bb55e6fcd9df9f90728e56b4f87aa92b22fe3b63df33f18a3188",
					hostPath:      "/var/run/foo.sock",
					vmPath:        "/run/socketfwd/954a6df32e91bb55e6fcd9df9f90728e56b4f87aa92b22fe3b63df33f18a3188.sock",
					containerPath: "/var/run/bar.sock",
				},
				{
					id:            "79a1c3c374d3573c0e7f1e7ca567d3531b3aefd3c202b8811df64e77fdaeab0c",
					hostPath:      "/var/run/abc.sock",
					vmPath:        "/run/socketfwd/79a1c3c374d3573c0e7f1e7ca567d3531b3aefd3c202b8811df64e77fdaeab0c.sock",
					containerPath: "/var/run/def.sock",
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mounts := append([]specs.Mount(nil), tc.mounts...)
			b := &bundle.Bundle{Spec: specs.Spec{Mounts: mounts}}
			p := &socketForwardsProvider{containerID: tc.cid}

			err := p.FromBundle(ctx, b)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantMounts, b.Spec.Mounts)
			assert.Equal(t, tc.wantEntries, p.entries)
		})
	}
}

// TestCreateRootfsPlaceholders_ConfinesToRootfs verifies that a UDS mount
// destination containing ".." components cannot escape sourceRootfs when
// the placeholder file is created — a regression test for a path-traversal
// issue where a plain filepath.Join(sourceRootfs, containerPath) would
// resolve ".." segments right out of sourceRootfs.
func TestCreateRootfsPlaceholders_ConfinesToRootfs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sourceRootfs := filepath.Join(root, "rootfs")
	require.NoError(t, os.MkdirAll(sourceRootfs, 0o755))

	p := &socketForwardsProvider{
		entries: []socketForwardEntry{
			{containerPath: "/../../etc/escaped.sock"},
			{containerPath: "/run/normal.sock"},
		},
	}

	p.CreateRootfsPlaceholders(ctx, sourceRootfs)

	// Neither placeholder should have escaped sourceRootfs: walk the
	// entire temp dir tree and confirm every created file is contained
	// within sourceRootfs.
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		require.NoError(t, err)
		if info.IsDir() || path == sourceRootfs {
			return nil
		}
		rel, err := filepath.Rel(sourceRootfs, path)
		require.NoError(t, err)
		assert.False(t, len(rel) >= 2 && rel[:2] == "..",
			"file %q escaped sourceRootfs %q", path, sourceRootfs)
		return nil
	})
	require.NoError(t, err)

	// The well-behaved mount's placeholder must still be created normally.
	assert.FileExists(t, filepath.Join(sourceRootfs, "run", "normal.sock"))
}

// TestUDSPlaceholderSource covers the mount-shape decision at the heart of
// the sandboxed UDS placeholder fix: only a rootfs whose *last* mount spec
// (the one mountutil.All actually mounts at the assembled path) is a
// read-only bind needs its placeholder written to that mount's Source
// before ShareRootfs runs. Every other shape — in particular a multi-entry
// overlay/erofs assembly, which is what a real snapshotter or this repo's
// erofs layer format actually hands Task.Create — has no single mount
// whose Source is the final assembled tree, so the assembled rootfs path
// itself is the only correct, and only available-after-ShareRootfs, target.
func TestUDSPlaceholderSource(t *testing.T) {
	const assembled = "/state/containers/ctr-1/rootfs"

	testcases := []struct {
		name           string
		mounts         []*types.Mount
		wantPath       string
		wantBefore     bool
		wantPathReason string
	}{
		{
			name:           "no mounts",
			mounts:         nil,
			wantPath:       assembled,
			wantBefore:     false,
			wantPathReason: "empty rootfs still assembles an (empty) directory at the guest path",
		},
		{
			name: "read-only bind (non-root shimtest / a committed snapshot)",
			mounts: []*types.Mount{
				{Type: "bind", Source: "/tmp/extracted-rootfs", Options: []string{"ro", "rbind"}},
			},
			wantPath:       "/tmp/extracted-rootfs",
			wantBefore:     true,
			wantPathReason: "ShareRootfs will mount this Source read-only at the assembled path",
		},
		{
			name: "writable bind (no ro option)",
			mounts: []*types.Mount{
				{Type: "bind", Source: "/tmp/writable-rootfs", Options: []string{"rbind"}},
			},
			wantPath:       assembled,
			wantBefore:     false,
			wantPathReason: "the bind stays writable, so using the assembled path (equivalent content) after ShareRootfs is correct and simpler",
		},
		{
			name: "bind marked ro but missing Source",
			mounts: []*types.Mount{
				{Type: "bind", Source: "", Options: []string{"ro"}},
			},
			wantPath:       assembled,
			wantBefore:     false,
			wantPathReason: "an empty Source can't be written to before assembly; fall back to the assembled path",
		},
		{
			name: "overlay/erofs multi-layer assembly (the common CRI/erofs shape)",
			mounts: []*types.Mount{
				{Type: "ext4", Source: "/state/scratch.ext4", Options: []string{"rw", "loop"}},
				{Type: "erofs", Source: "/layers/base.erofs", Options: []string{"ro", "loop"}},
				{
					Type:   "format/mkdir/overlay",
					Source: "overlay",
					Options: []string{
						"workdir={{ mount 0 }}/work",
						"upperdir={{ mount 0 }}/upper",
						"lowerdir={{ mount 1 }}",
					},
				},
			},
			wantPath:       assembled,
			wantBefore:     false,
			wantPathReason: "no single mount's Source is the assembled tree; the overlay's writable upperdir backs the assembled path itself",
		},
		{
			name: "single overlay mount with explicit upperdir",
			mounts: []*types.Mount{
				{
					Type:    "overlay",
					Source:  "overlay",
					Options: []string{"lowerdir=/l1:/l2", "upperdir=/upper", "workdir=/work"},
				},
			},
			wantPath:       assembled,
			wantBefore:     false,
			wantPathReason: "an overlay mount is never type \"bind\", so it must resolve to the assembled path",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			gotPath, gotBefore := udsPlaceholderSource(tc.mounts, assembled)
			assert.Equal(t, tc.wantPath, gotPath, tc.wantPathReason)
			assert.Equal(t, tc.wantBefore, gotBefore)
		})
	}
}

// TestCreateRootfsPlaceholders_OverlayShapedRootfs is an end-to-end
// regression test for the bug identified in review: previously, placeholder
// creation only ever scanned r.Rootfs for a "bind"-typed entry, so an
// overlay/erofs-shaped rootfs (no "bind" entry at all — the shape used by
// the erofs snapshotter and any real CRI overlay snapshotter) produced zero
// placeholders, leaving a UDS mount's rewritten bind destination missing.
//
// This test drives the same two-call sequence service.go's
// createSandboxedContainer uses (udsPlaceholderSource to pick a target,
// then CreateRootfsPlaceholders) against an overlay-shaped mount list and a
// writable directory standing in for the host path SharedFS.ShareRootfs
// would have assembled, and asserts the placeholder lands there.
func TestCreateRootfsPlaceholders_OverlayShapedRootfs(t *testing.T) {
	ctx := context.Background()
	assembledRootfs := t.TempDir()

	overlayShapedMounts := []*types.Mount{
		{Type: "ext4", Source: "/state/scratch.ext4", Options: []string{"rw", "loop"}},
		{Type: "erofs", Source: "/layers/base.erofs", Options: []string{"ro", "loop"}},
		{Type: "format/mkdir/overlay", Source: "overlay", Options: []string{
			"workdir={{ mount 0 }}/work",
			"upperdir={{ mount 0 }}/upper",
			"lowerdir={{ mount 1 }}",
		}},
	}

	p := &socketForwardsProvider{
		entries: []socketForwardEntry{
			{containerPath: "/run/shared.sock"},
		},
	}

	placeholderSrc, beforeAssembly := udsPlaceholderSource(overlayShapedMounts, assembledRootfs)
	require.False(t, beforeAssembly, "an overlay-shaped rootfs has no writable Source available before assembly")
	require.Equal(t, assembledRootfs, placeholderSrc)

	// Mirror service.go: this call only happens after ShareRootfs would
	// have assembled the rootfs (here, simply because assembledRootfs
	// already exists and is writable).
	p.CreateRootfsPlaceholders(ctx, placeholderSrc)

	assert.FileExists(t, filepath.Join(assembledRootfs, "run", "shared.sock"),
		"UDS placeholder must be created in the assembled rootfs when no mount entry has a usable pre-assembly Source")
}
