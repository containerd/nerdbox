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

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

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
		wantInitArgs    []string
	}{
		{
			name:            "no mounts",
			mounts:          nil,
			wantMounts:      nil,
			wantSpecSources: nil,
			wantInitArgs:    []string{},
		},
		{
			name: "no bind mounts",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantMounts:      nil,
			wantSpecSources: []string{"tmpfs", "proc"},
			wantInitArgs:    []string{},
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
					vmTarget: "/mnt/bind-8c5eaa445dd84f17",
				},
			},
			wantSpecSources: []string{"/mnt/bind-8c5eaa445dd84f17"},
			wantInitArgs:    []string{"-mount=bind-8c5eaa445dd84f17:/mnt/bind-8c5eaa445dd84f17"},
		},
		{
			name: "multiple bind mounts",
			mounts: []specs.Mount{
				{Type: "bind", Source: testdirData, Destination: "/container/data"},
				{Type: "bind", Source: testdirConfig, Destination: "/container/config"},
			},
			wantMounts: []bindMount{
				{
					tag:      "bind-8c5eaa445dd84f17",
					hostSrc:  testdirData,
					vmTarget: "/mnt/bind-8c5eaa445dd84f17",
				},
				{
					tag:      "bind-529984c9ac58b7ec",
					hostSrc:  testdirConfig,
					vmTarget: "/mnt/bind-529984c9ac58b7ec",
				},
			},
			wantSpecSources: []string{
				"/mnt/bind-8c5eaa445dd84f17",
				"/mnt/bind-529984c9ac58b7ec",
			},
			wantInitArgs: []string{
				"-mount=bind-8c5eaa445dd84f17:/mnt/bind-8c5eaa445dd84f17",
				"-mount=bind-529984c9ac58b7ec:/mnt/bind-529984c9ac58b7ec",
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
					vmTarget: "/mnt/bind-8c5eaa445dd84f17",
				},
			},
			wantSpecSources: []string{
				"tmpfs",
				"/mnt/bind-8c5eaa445dd84f17",
				"proc",
			},
			wantInitArgs: []string{"-mount=bind-8c5eaa445dd84f17:/mnt/bind-8c5eaa445dd84f17"},
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
					vmTarget: "/mnt/bind-6dace5108a719565",
				},
			},
			wantSpecSources: []string{"/mnt/bind-6dace5108a719565/testfile.txt"},
			wantInitArgs:    []string{"-mount=bind-6dace5108a719565:/mnt/bind-6dace5108a719565"},
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

			// Verify the args passed to vminitd
			args := bm.InitArgs()
			assert.Equal(t, tc.wantInitArgs, args)
		})
	}
}
