//go:build linux

package tracing

import (
	"context"

	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/nerdbox/internal/tracing"
	vmtracing "github.com/containerd/nerdbox/internal/vminit/tracing"
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

			collector := tracing.NewCollector()
			tracing.SetSink(collector)

			ss.(shutdown.Service).RegisterCallback(func(ctx context.Context) error {
				tracing.SetSink(nil)
				collector.Shutdown()
				return nil
			})

			return vmtracing.NewService(collector), nil
		},
	})
}
