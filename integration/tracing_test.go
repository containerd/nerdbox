//go:build tracing

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

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	bundleapi "github.com/containerd/nerdbox/api/services/bundle/v1"
	systemapi "github.com/containerd/nerdbox/api/services/system/v1"
	"github.com/containerd/nerdbox/internal/vm"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// TestTraceVMBoot boots a VM and exercises the TTRPC API with OTel tracing
// exported to a local Jaeger instance (localhost:4318).
//
// Run with: make test-tracing
func TestTraceVMBoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// Set up OTLP exporter pointing at Jaeger.
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint("localhost:4318"),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		t.Fatal("creating OTLP exporter:", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := exp.Shutdown(shutdownCtx); err != nil {
			t.Logf("exporter shutdown: %v", err)
		}
	})

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("nerdbox"),
		),
	)
	if err != nil {
		t.Fatal("creating OTel resource:", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			t.Logf("tracer provider shutdown: %v", err)
		}
	}()

	tracer := otel.Tracer("nerdbox-test")
	ctx, rootSpan := tracer.Start(ctx, "TestTraceVMBoot")
	defer rootSpan.End()

	for _, backend := range vmBackends {
		t.Run(backend.name, func(t *testing.T) {
			traceVMBoot(ctx, t, tracer, backend.vmm)
		})
	}

	// Force flush so all spans are sent to Jaeger before test ends.
	if err := tp.ForceFlush(ctx); err != nil {
		t.Logf("traces export: %v (expected when no collector is running)", err)
	}

	t.Log("Traces sent to Jaeger. Open http://localhost:16686 to view.")
}

func traceVMBoot(ctx context.Context, t *testing.T, tracer trace.Tracer, vmm vm.Manager) {
	td := t.TempDir()
	t.Chdir(td)
	// Use Getwd to resolve symlinks (e.g., /var -> /private/var on macOS)
	resolvedTd, err := os.Getwd()
	if err != nil {
		t.Fatal("failed to resolve temp dir:", err)
	}

	// Span: VM.NewInstance+Start
	vmCtx, vmSpan := tracer.Start(ctx, "VM.NewInstance+Start")
	instance, err := vmm.NewInstance(vmCtx, resolvedTd)
	if err != nil {
		vmSpan.End()
		t.Fatal("failed to create VM instance:", err)
	}
	if err := instance.Start(vmCtx); err != nil {
		vmSpan.End()
		t.Fatal("failed to start VM:", err)
	}
	vmSpan.End()

	t.Cleanup(func() {
		instance.Shutdown(t.Context())
	})

	client := instance.Client()

	// Span: TTRPC.System.Info
	infoCtx, infoSpan := tracer.Start(ctx, "TTRPC.System.Info")
	ss := systemapi.NewTTRPCSystemClient(client)
	resp, err := ss.Info(infoCtx, nil)
	infoSpan.End()
	if err != nil {
		t.Fatal("failed to get system info:", err)
	}
	t.Logf("System info: version=%s kernel=%s", resp.Version, resp.KernelVersion)

	// Span: TTRPC.Bundle.Create
	bundleCtx, bundleSpan := tracer.Start(ctx, "TTRPC.Bundle.Create")
	bs := bundleapi.NewTTRPCBundleClient(client)
	_, err = bs.Create(bundleCtx, &bundleapi.CreateRequest{
		ID: "trace-test-bundle",
	})
	bundleSpan.End()
	if err != nil {
		t.Fatal("failed to create bundle:", err)
	}
	t.Log("Bundle created successfully")
}
