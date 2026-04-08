package tracing

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/containerd/log"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Flusher collects finished spans and periodically exports them via OTLP HTTP.
type Flusher struct {
	endpoint    string
	serviceName string
	client      *http.Client

	mu      sync.Mutex
	pending []*Span
	done    chan struct{}
}

// NewFlusher creates a flusher that exports to the given OTLP endpoint.
// Call Shutdown to flush remaining spans and stop the background goroutine.
func NewFlusher(ctx context.Context, endpoint, serviceName string, interval time.Duration) *Flusher {
	f := &Flusher{
		endpoint:    endpoint,
		serviceName: serviceName,
		client:      &http.Client{},
		done:        make(chan struct{}),
	}
	go f.loop(ctx, interval)
	return f
}

// Collect adds a finished span to the pending batch. Never blocks.
func (f *Flusher) Collect(s *Span) {
	f.mu.Lock()
	f.pending = append(f.pending, s)
	f.mu.Unlock()
}

// Shutdown flushes remaining spans and stops the background goroutine.
func (f *Flusher) Shutdown(ctx context.Context) error {
	close(f.done)
	return f.flush(ctx)
}

func (f *Flusher) loop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := f.flush(ctx); err != nil {
				log.G(ctx).WithError(err).Warn("trace flush")
			}
		case <-f.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (f *Flusher) flush(ctx context.Context) error {
	f.mu.Lock()
	spans := f.pending
	f.pending = nil
	f.mu.Unlock()

	if len(spans) == 0 {
		return nil
	}

	protoSpans := make([]*tracepb.Span, 0, len(spans))
	for _, s := range spans {
		protoSpans = append(protoSpans, spanToOTLP(s))
	}

	req := &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{{
					Key:   "service.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: f.serviceName}},
				}},
			},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: protoSpans,
			}},
		}},
	}

	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal OTLP request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, f.endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")

	resp, err := f.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send OTLP request: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("OTLP export failed: %s", resp.Status)
	}
	return nil
}

func spanToOTLP(s *Span) *tracepb.Span {
	return &tracepb.Span{
		TraceId:           s.TraceID[:],
		SpanId:            s.SpanID[:],
		ParentSpanId:      s.ParentSpanID[:],
		Name:              s.Name,
		Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: uint64(s.StartTime.UnixNano()),
		EndTimeUnixNano:   uint64(s.EndTime.UnixNano()),
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
	}
}

// OTLPEndpoint returns the OTLP traces endpoint URL derived from
// OTEL_EXPORTER_OTLP_ENDPOINT, or "" if the env var is unset.
func OTLPEndpoint() string {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return ""
	}
	if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" {
		endpoint = "http://" + endpoint
	}
	return endpoint + "/v1/traces"
}

// Init sets up the global span sink with an OTLP HTTP flusher.
// Returns a shutdown function. If OTEL_EXPORTER_OTLP_ENDPOINT is unset,
// tracing is disabled and the returned shutdown is a no-op.
func Init(ctx context.Context, serviceName string) func(context.Context) error {
	endpoint := OTLPEndpoint()
	if endpoint == "" {
		return func(context.Context) error { return nil }
	}
	f := NewFlusher(ctx, endpoint, serviceName, 100*time.Millisecond)
	SetSink(f)
	log.G(ctx).WithField("endpoint", endpoint).Debug("tracing enabled")
	return func(ctx context.Context) error {
		SetSink(nil)
		return f.Shutdown(ctx)
	}
}
