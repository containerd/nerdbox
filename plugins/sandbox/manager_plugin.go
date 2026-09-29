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

package sandbox

import (
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
	vmsbox "github.com/containerd/nerdbox/internal/shim/sandbox/vm"
	"github.com/containerd/nerdbox/pkg/vm"
	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.SandboxPlugin,
		ID:   "manager",
		Requires: []plugin.Type{
			plugins.VMManagerPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			// Only a single VM manager plugin is supported.
			vmm, err := ic.GetSingle(plugins.VMManagerPlugin)
			if err != nil {
				return nil, err
			}
			sb := vmsbox.NewVMSandbox(vmm.(vm.Manager))
			return sandbox.NewSandboxService(sb), nil
		},
	})
}
