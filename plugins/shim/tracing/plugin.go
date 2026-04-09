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

package tracing

import (
	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	"github.com/containerd/nerdbox/internal/tracing"
	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "tracing",
		Requires: []plugin.Type{
			cplugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ctx := ic.Context

			shutdownTracing := tracing.Init(ctx, "nerdbox")
			if tracing.ParseOTLPEndpoint() == nil {
				log.G(ctx).Debug("OTEL_EXPORTER_OTLP_ENDPOINT not set, shim tracing disabled")
			}

			ss, err := ic.GetByID(cplugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			ss.(shutdown.Service).RegisterCallback(shutdownTracing)

			return &interceptor{}, nil
		},
	})
}

type interceptor struct{}

func (i *interceptor) UnaryServerInterceptor() ttrpc.UnaryServerInterceptor {
	return tracing.UnaryServerInterceptor()
}
