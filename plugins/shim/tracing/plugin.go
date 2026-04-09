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
