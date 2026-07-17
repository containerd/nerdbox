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
