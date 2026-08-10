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

package manager

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/shim"
)

// MountNSIsolatedEnv is set (to "1") on the shim server child's own
// environment whenever cloneMntNs actually isolated its mount namespace
// (CLONE_NEWNS). The shim's own main, once running as that child, checks
// for this and calls IsolateMountPropagation if it's set — see that
// function's doc comment for why the isolation itself isn't enough
// without it.
//
// An env var rather than, say, a CLI flag or cloneMntNs's own return value:
// it needs to reach the child process specifically (not whichever
// invocation of the shim binary containerd made directly, e.g. its
// separate "start"/"delete" actions, which never go through cloneMntNs at
// all), and cloneMntNs's boolean return is already used by its caller for
// an unrelated purpose (whether to retry without a user namespace).
const MountNSIsolatedEnv = "NERDBOX_MOUNT_NS_ISOLATED"

// New returns a shim manager implementation that launches the nerdbox shim
// process. The name is the runtime identifier reported to containerd (for
// example "io.containerd.nerdbox.v1"). External callers building variants
// of the nerdbox shim use this to assemble their own main package.
func New(name string) shim.Shim {
	return &manager{
		name: name,
	}
}

type manager struct {
	name string
}

// group labels specifies how the shim groups services.
// currently supports a runc.v2 specific .group label and the
// standard k8s pod label.  Order matters in this list
var groupLabels = []string{
	"io.containerd.runc.v2.group",
	"io.kubernetes.cri.sandbox-id",
}

// spec is a shallow version of [oci.Spec] containing only the
// fields we need for the hook. We use a shallow struct to reduce
// the overhead of unmarshaling.
type spec struct {
	// Annotations contains arbitrary metadata for the container.
	Annotations map[string]string `json:"annotations,omitempty"`
}

func readSpec() (*spec, error) {
	const configFileName = "config.json"
	f, err := os.Open(configFileName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var s spec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m manager) Name() string {
	return m.name
}

func (m manager) Info(ctx context.Context, optionsR io.Reader) (*types.RuntimeInfo, error) {
	info := &types.RuntimeInfo{
		Name:    m.name,
		Version: &types.RuntimeVersion{
			//Version:  version.Version,
			//Revision: version.Revision,
		},
		Annotations: map[string]string{
			"containerd.io/runtime-allow-mounts": "mkdir/*,format/*,erofs",
		},
	}
	// TODO: Get features list from run_vminitd
	/*
		opts, err := shim.ReadRuntimeOptions[*options.Options](optionsR)
		if err != nil {
			if !errors.Is(err, errdefs.ErrNotFound) {
				return nil, fmt.Errorf("failed to read runtime options (*options.Options): %w", err)
			}
		}
		if opts != nil {
			info.Options, err = typeurl.MarshalAnyToProto(opts)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal %T: %w", opts, err)
			}
			// TODO: use opts.BinaryName

		}
	*/
	return info, nil
}
