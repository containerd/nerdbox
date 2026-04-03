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

// Package tracing relays OTel spans from a VM to the host.
package tracing

import (
	"context"
	"net/url"
	"os"
	"time"

	"github.com/containerd/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	tracespb "github.com/containerd/nerdbox/api/services/traces/v1"
)

var vmResource = resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("nerdbox-vm"))

// ForwardTraces reads spans from the VM trace stream and re-exports them
// on the host via the provided exporter. hostBootTime is the host
// wall-clock time captured when ttrpc became responsive, used to correct
// VM-vs-host clock skew (caused by the VM's RTC having only second-level
// resolution).
func ForwardTraces(ctx context.Context, stream tracespb.TTRPCTraces_StreamClient, exporter sdktrace.SpanExporter, hostBootTime time.Time) {
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := exporter.Shutdown(shutdownCtx); err != nil {
			log.G(ctx).WithError(err).Warn("trace relay exporter shutdown")
		}
	}()

	// The VM's RTC has only second-level resolution, so its wall clock
	// can be up to ~1s behind the host. We compute the offset from the
	// first otelttrpc interceptor span (which is created at the moment
	// the first ttrpc RPC reaches the VM — a known sync point with the
	// host). hostBootTime was captured on the host at the same logical
	// moment (when ttrpc became responsive).
	var clockOffset time.Duration
	offsetComputed := false

	for {
		span, err := stream.Recv()
		if err != nil {
			log.G(ctx).WithError(err).Debug("trace stream ended")
			return
		}

		if !offsetComputed {
			vmTime := time.Unix(0, span.StartTimeUnixNano)
			clockOffset = hostBootTime.Sub(vmTime)
			offsetComputed = true
			log.G(ctx).WithField("offset", clockOffset).Debug("VM clock offset computed")
		}

		stub := protoToSpanStub(span, clockOffset)
		snapshot := stub.Snapshot()
		if err := exporter.ExportSpans(ctx, []sdktrace.ReadOnlySpan{snapshot}); err != nil {
			log.G(ctx).WithError(err).Warn("trace relay export")
		}
	}
}

func protoToSpanStub(s *tracespb.Span, clockOffset time.Duration) tracetest.SpanStub {
	var tid trace.TraceID
	var sid trace.SpanID
	var psid trace.SpanID

	copy(tid[:], s.TraceID)
	copy(sid[:], s.SpanID)
	copy(psid[:], s.ParentSpanID)

	var attrs []attribute.KeyValue
	for _, kv := range s.Attributes {
		attrs = append(attrs, attribute.String(kv.Key, kv.Value))
	}

	return tracetest.SpanStub{
		Name: s.Name,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    tid,
			SpanID:     sid,
			TraceFlags: trace.FlagsSampled,
		}),
		Parent: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    tid,
			SpanID:     psid,
			TraceFlags: trace.FlagsSampled,
		}),
		SpanKind:   trace.SpanKind(s.Kind),
		StartTime:  time.Unix(0, s.StartTimeUnixNano).Add(clockOffset),
		EndTime:    time.Unix(0, s.EndTimeUnixNano).Add(clockOffset),
		Attributes: attrs,
		Status: sdktrace.Status{
			Code:        codes.Code(s.StatusCode),
			Description: s.StatusMessage,
		},
		Resource: vmResource,
	}
}

// NewRelayExporter creates an OTLP HTTP exporter from the
// OTEL_EXPORTER_OTLP_ENDPOINT env var. Returns nil if the env var is unset.
func NewRelayExporter(ctx context.Context) sdktrace.SpanExporter {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(endpoint),
	}
	if u, err := url.Parse(endpoint); err == nil && u.Scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		log.G(ctx).WithError(err).WithField("endpoint", endpoint).Warn("failed to create trace relay exporter")
		return nil
	}
	log.G(ctx).WithField("endpoint", endpoint).Debug("trace relay exporter created")
	return exp
}
