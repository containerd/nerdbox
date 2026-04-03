//go:build linux

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
	"context"
	"time"

	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/containerd/nerdbox/internal/vminit/tracing"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "traces",
		Requires: []plugin.Type{
			cplugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ss, err := ic.GetByID(cplugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}

			exp := tracing.NewExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp,
				sdktrace.WithBatchTimeout(100*time.Millisecond),
			))
			otel.SetTracerProvider(tp)
			otel.SetTextMapPropagator(propagation.TraceContext{})

			ss.(shutdown.Service).RegisterCallback(func(ctx context.Context) error {
				return tp.Shutdown(ctx)
			})

			return tracing.NewService(exp), nil
		},
	})
}
