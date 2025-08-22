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

// Package libkrun provides a plugin for creating vm using libkrun
package libkrun

import (
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/dmcgowan/nerdbox/internal/vm/libkrun"
	"github.com/dmcgowan/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type:     plugins.VMManagerPlugin,
		ID:       "libkrun",
		Requires: []plugin.Type{},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			return libkrun.NewManager(), nil
		},
	})
}
