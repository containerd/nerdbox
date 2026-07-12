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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxStartOptions(t *testing.T) {
	t.Run("missing config.json falls back to resource defaults, not an error", func(t *testing.T) {
		opts, err := SandboxStartOptions(false)(context.Background(), t.TempDir())
		require.NoError(t, err)
		assert.NotEmpty(t, opts)
	})

	t.Run("config.json without Root (the real sandbox-bundle shape) is not an error", func(t *testing.T) {
		// A sandbox bundle's config.json legitimately has no Root at all —
		// a sandbox has no rootfs of its own — unlike a container bundle,
		// where a missing Root is a real error. This is the actual shape
		// containerd's sandbox controller and shimtest's sandbox suite
		// both produce, so this case must go through LoadSandboxConfig
		// successfully rather than being treated the same as a container
		// bundle missing Root.
		spec := specs.Spec{
			Annotations: map[string]string{
				"io.containerd.nerdbox.resources.cpu":    "4",
				"io.containerd.nerdbox.resources.memory": "4096",
			},
		}
		dir := t.TempDir()
		data, err := json.Marshal(spec)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644))

		opts, err := SandboxStartOptions(false)(context.Background(), dir)
		require.NoError(t, err)
		assert.NotEmpty(t, opts)
	})

	t.Run("malformed config.json is an error, not a silent fallback", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte("not json"), 0o644))

		_, err := SandboxStartOptions(false)(context.Background(), dir)
		assert.Error(t, err)
	})

	t.Run("a transformer error (bad network annotation) is an error, not a silent fallback", func(t *testing.T) {
		spec := specs.Spec{
			Root: &specs.Root{Path: "rootfs"},
			Annotations: map[string]string{
				"io.containerd.nerdbox.network.0": "not-a-valid-field",
			},
		}
		dir := t.TempDir()
		data, err := json.Marshal(spec)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644))

		_, err = SandboxStartOptions(false)(context.Background(), dir)
		assert.Error(t, err)
	})
}
