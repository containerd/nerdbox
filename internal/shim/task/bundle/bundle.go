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

package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

type Bundle struct {
	Path   string // Path is the bundle path.
	Spec   specs.Spec
	Rootfs string // Rootfs is the absolute path to the root filesystem.

	// extraFiles are files that are not part of the OCI bundle but are needed
	// to setup containers in the VM. Keep it unexported to force consumers to
	// call Files to get all the files, including the updated OCI spec.
	extraFiles map[string][]byte
}

type Transformer func(ctx context.Context, b *Bundle) error

// Load loads a container's OCI bundle from the given path and applies a
// series of transformers to turn the host-side bundle into a VM-side
// bundle. A container bundle always has a Root, so its absence is an error;
// see LoadSandboxConfig for the sandbox-bundle case, which has no rootfs of
// its own.
func Load(ctx context.Context, path string, transformers ...Transformer) (*Bundle, error) {
	return load(ctx, path, true, transformers...)
}

// LoadSandboxConfig loads the sandbox-level bundle at path — the one
// containerd's sandbox controller passes in CreateSandboxRequest.BundlePath,
// used only to derive VM start options (resources, networking) ahead of any
// container running in it — and applies transformers the same way Load
// does. Unlike a container bundle, a sandbox bundle has no rootfs of its
// own, so a missing Root in its config.json is expected, not an error.
func LoadSandboxConfig(ctx context.Context, path string, transformers ...Transformer) (*Bundle, error) {
	return load(ctx, path, false, transformers...)
}

func load(ctx context.Context, path string, rootRequired bool, transformers ...Transformer) (*Bundle, error) {
	specBytes, err := os.ReadFile(filepath.Join(path, "config.json"))
	if err != nil {
		return nil, err
	}

	b := Bundle{
		Path:       path,
		extraFiles: make(map[string][]byte),
	}
	if err := json.Unmarshal(specBytes, &b.Spec); err != nil {
		return nil, err
	}

	if err := resolveRootfsPath(ctx, &b, rootRequired); err != nil {
		return nil, err
	}

	for _, t := range transformers {
		if err := t(ctx, &b); err != nil {
			return nil, err
		}
	}

	return &b, nil
}

func (b *Bundle) AddExtraFile(name string, data []byte) {
	b.extraFiles[name] = data
}

// Files returns all the bundle files that must be setup inside the VM.
func (b *Bundle) Files() (map[string][]byte, error) {
	files := maps.Clone(b.extraFiles)

	specBytes, err := json.Marshal(b.Spec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal spec: %w", err)
	}
	files["config.json"] = specBytes

	return files, nil
}

func resolveRootfsPath(ctx context.Context, b *Bundle, required bool) error {
	if b.Spec.Root == nil {
		if required {
			return fmt.Errorf("root path not specified: %w", errdefs.ErrInvalidArgument)
		}
		return nil
	}

	if filepath.IsAbs(b.Spec.Root.Path) {
		b.Rootfs = b.Spec.Root.Path
	} else {
		b.Rootfs = filepath.Join(b.Path, b.Spec.Root.Path)
	}
	b.Spec.Root.Path = "rootfs"

	return nil
}
