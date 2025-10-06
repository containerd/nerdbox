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

// Load loads an OCI bundle from the given path and apply a series of transformers
// to turn the host-side bundle into a VM-side bundle.
func Load(ctx context.Context, path string, transformers ...Transformer) (*Bundle, error) {
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

	if err := resolveRootfsPath(ctx, &b); err != nil {
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

func resolveRootfsPath(ctx context.Context, b *Bundle) error {
	if b.Spec.Root == nil {
		return fmt.Errorf("root path not specified: %w", errdefs.ErrInvalidArgument)
	}

	if filepath.IsAbs(b.Spec.Root.Path) {
		b.Rootfs = b.Spec.Root.Path
	} else {
		b.Rootfs = filepath.Join(b.Path, b.Spec.Root.Path)
	}
	b.Spec.Root.Path = "rootfs"

	return nil
}
