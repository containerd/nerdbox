//go:build linux

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

package runc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSpec(t *testing.T, spec *specs.Spec) string {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, json.NewEncoder(f).Encode(spec))
	return dir
}

func TestShouldKillAllOnExit(t *testing.T) {
	ctx := context.Background()

	testcases := []struct {
		name string
		spec *specs.Spec
		want bool
	}{
		{
			name: "private PID namespace (empty Path): kernel already reaped everything",
			spec: &specs.Spec{Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace, Path: ""},
			}}},
			want: false,
		},
		{
			name: "shared PID namespace (non-empty Path): must kill explicitly",
			spec: &specs.Spec{Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace, Path: "/run/pidns/some-sandbox"},
			}}},
			want: true,
		},
		{
			name: "no PID namespace entry at all (host PID): must kill explicitly",
			spec: &specs.Spec{Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace},
			}}},
			want: true,
		},
		{
			name: "nil Linux: must kill explicitly",
			spec: &specs.Spec{},
			want: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeSpec(t, tc.spec)
			assert.Equal(t, tc.want, ShouldKillAllOnExit(ctx, dir))
		})
	}
}

// TestShouldKillAllOnExit_MissingSpec verifies the fail-safe default: a
// bundle whose config.json can't be read is treated the same as "must kill
// explicitly", not silently skipped.
func TestShouldKillAllOnExit_MissingSpec(t *testing.T) {
	assert.True(t, ShouldKillAllOnExit(context.Background(), t.TempDir()))
}
