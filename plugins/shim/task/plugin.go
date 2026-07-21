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
	"fmt"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	intsandbox "github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task"
	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TaskPlugin,
		ID:   "manager",
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

			svc, ok := sbRaw.(*intsandbox.SandboxService)
			if !ok {
				return nil, fmt.Errorf("unexpected SandboxPlugin implementation %T", sbRaw)
			}

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

	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.TaskPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			tPlugin, err := ic.GetSingle(plugins.TaskPlugin)
			if err != nil {
				return nil, err
			}

			tm, ok := tPlugin.(taskAPI.TTRPCTaskService)
			if !ok {
				return nil, fmt.Errorf("unexpected task plugin implementation %T", tPlugin)
			}

			return taskService{srv: tm}, nil
		},
	})
}

type taskService struct {
	srv taskAPI.TTRPCTaskService
}

func (s taskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s.srv)
	return nil
}
