// Copyright The containerd Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package task

import (
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	intsandbox "github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task"
	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			cplugins.EventPlugin,
			cplugins.InternalPlugin,
			plugins.SandboxPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			pp, err := ic.GetByID(cplugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			ss, err := ic.GetByID(cplugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			sbRaw, err := ic.GetSingle(plugins.SandboxPlugin)
			if err != nil {
				return nil, err
			}

			// Unwrap the sandboxManager to get the underlying SandboxService.
			type sandboxManagerUnwrapper interface {
				Service() *intsandbox.SandboxService
			}
			svc := sbRaw.(sandboxManagerUnwrapper).Service()

			// Determine debug flag from shim opts stored in context.
			debug := false
			if opts, ok := ic.Context.Value(shim.OptsKey{}).(shim.Opts); ok {
				debug = opts.Debug
			}

			// Wire the bundle-derived VM start options callback into the
			// SandboxService so that StartSandbox can boot the VM with the
			// correct resources and networking without importing the task package.
			svc.RegisterStartOptions(task.SandboxStartOptions(debug))

			return task.NewTaskService(ic.Context, svc, pp.(shim.Publisher), ss.(shutdown.Service))
		},
	})
}
