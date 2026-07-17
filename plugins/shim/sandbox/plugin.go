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
	"context"
	"net"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	intsandbox "github.com/containerd/nerdbox/internal/shim/sandbox"
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
			// Wrap the raw Sandbox in a SandboxService that implements
			// both the Sandbox interface and the containerd
			// TTRPCSandboxService. The SandboxPlugin does NOT implement
			// shim.TTRPCService — TTRPC registration is handled by the
			// dedicated TTRPCPlugin "sandbox" in service_plugin.go. This
			// prevents a double-registration panic when the shim framework
			// iterates all plugins looking for TTRPCService implementors.
			return &sandboxManager{svc: intsandbox.NewSandboxService(sb)}, nil
		},
	})
}

// sandboxManager wraps *intsandbox.SandboxService and exposes the
// intsandbox.Sandbox interface to the plugin system while intentionally NOT
// implementing shim.TTRPCService. This prevents the shim framework from
// calling RegisterTTRPC on the SandboxPlugin instance, which would cause a
// duplicate registration panic (the TTRPCPlugin "sandbox" handles that).
type sandboxManager struct {
	svc *intsandbox.SandboxService
}

// Verify that sandboxManager satisfies the Sandbox interface.
var _ intsandbox.Sandbox = (*sandboxManager)(nil)

// Service returns the underlying *intsandbox.SandboxService. The task and
// TTRPC-sandbox plugins use this to access sandbox-specific operations.
func (m *sandboxManager) Service() *intsandbox.SandboxService {
	return m.svc
}

// Export NetNS
// Export Options

// The following methods delegate to the underlying SandboxService so that
// sandboxManager satisfies intsandbox.Sandbox (required by the streaming
// plugin and any other consumer of the SandboxPlugin value).

func (m *sandboxManager) Start(ctx context.Context, opts ...intsandbox.Opt) error {
	return m.svc.Start(ctx, opts...)
}

func (m *sandboxManager) Stop(ctx context.Context) error {
	return m.svc.Stop(ctx)
}

func (m *sandboxManager) Client() (*ttrpc.Client, error) {
	return m.svc.Client()
}

func (m *sandboxManager) StartStream(ctx context.Context, id string) (net.Conn, error) {
	return m.svc.StartStream(ctx, id)
}

func (m *sandboxManager) ReservedDisks() int {
	return m.svc.ReservedDisks()
}
