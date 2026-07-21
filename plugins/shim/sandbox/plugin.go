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
	"fmt"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

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

	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "sandbox",
		Requires: []plugin.Type{
			plugins.SandboxPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			sbPlugin, err := ic.GetSingle(plugins.SandboxPlugin)
			if err != nil {
				return nil, err
			}

			sm, ok := sbPlugin.(sandboxAPI.TTRPCSandboxService)
			if !ok {
				return nil, fmt.Errorf("unexpected sandbox plugin implementation %T", sbPlugin)
			}
			return &sbService{srv: sm}, nil
		},
	})
}

// sbService adapts a sandboxAPI.TTRPCSandboxService to shim.TTRPCService,
// so that the "sandbox" TTRPCPlugin registration above (rather than the
// SandboxPlugin "manager" registration, which other plugins such as
// streaming/transfer depend on as a plain sandbox.Sandbox) is the one the
// shim framework calls RegisterTTRPC on. Without this indirection, the
// framework would either not find a RegisterTTRPC method at all, or (if
// SandboxService implemented it directly) call it a second time when it
// scans the "manager" plugin's own instance, double-registering the
// service.
type sbService struct {
	srv sandboxAPI.TTRPCSandboxService
}

// RegisterTTRPC registers the sandbox service on the TTRPC server.
func (s *sbService) RegisterTTRPC(server *ttrpc.Server) error {
	sandboxAPI.RegisterTTRPCSandboxService(server, s.srv)
	return nil
}
