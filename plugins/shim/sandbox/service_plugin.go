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

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "sandbox",
		Requires: []plugin.Type{
			plugins.SandboxPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			sbRaw, err := ic.GetSingle(plugins.SandboxPlugin)
			if err != nil {
				return nil, err
			}
			// Unwrap the sandboxManager to get the *SandboxService.
			// The SandboxService implements both the Sandbox interface and the
			// containerd TTRPCSandboxService. Returning it here (as a
			// TTRPCPlugin) causes the shim framework to call RegisterTTRPC
			// exactly once, registering the sandbox TTRPC service.
			sm, ok := sbRaw.(*sandboxManager)
			if !ok {
				return nil, fmt.Errorf("unexpected SandboxPlugin implementation %T", sbRaw)
			}
			return sm.Service(), nil
		},
	})
}
