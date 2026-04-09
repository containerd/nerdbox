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
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/log"
)

// Flusher collects finished spans and periodically exports them via OTLP HTTP.
type Flusher struct {
	addr        string // host:port
	path        string // e.g. "/v1/traces"
	serviceName string

	mu      sync.Mutex
	pending []*Span
	done    chan struct{}
}

// NewFlusher creates a flusher that exports to the given OTLP endpoint.
// Call Shutdown to flush remaining spans and stop the background goroutine.
func NewFlusher(ctx context.Context, addr, path, serviceName string, interval time.Duration) *Flusher {
	f := &Flusher{
		addr:        addr,
		path:        path,
		serviceName: serviceName,
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

	otlpSpans := make([]otlpSpan, 0, len(spans))
	for _, s := range spans {
		otlpSpans = append(otlpSpans, spanToOTLPJSON(s))
	}

	req := otlpExportRequest{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{
				Attributes: []otlpKeyValue{{
					Key:   "service.name",
					Value: otlpAnyValue{StringValue: f.serviceName},
				}},
			},
			ScopeSpans: []otlpScopeSpans{{
				Spans: otlpSpans,
			}},
		}},
	}

	return postOTLP(f.addr, f.path, req)
}

func spanToOTLPJSON(s *Span) otlpSpan {
	return otlpSpan{
		TraceID:           s.TraceID.String(),
		SpanID:            s.SpanID.String(),
		ParentSpanID:      s.ParentSpanID.String(),
		Name:              s.Name,
		Kind:              1, // SPAN_KIND_INTERNAL
		StartTimeUnixNano: strconv.FormatInt(s.StartTime.UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(s.EndTime.UnixNano(), 10),
		Status:            otlpStatus{Code: 1}, // STATUS_CODE_OK
	}
}

// postOTLP sends an OTLP JSON request via a raw HTTP/1.1 POST over TCP.
// This avoids importing net/http (and its transitive crypto/tls stack).
func postOTLP(addr, path string, req otlpExportRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal OTLP request: %w", err)
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Write a minimal HTTP/1.1 request.
	var buf []byte
	buf = append(buf, "POST "...)
	buf = append(buf, path...)
	buf = append(buf, " HTTP/1.1\r\nHost: "...)
	buf = append(buf, addr...)
	buf = append(buf, "\r\nContent-Type: application/json\r\nContent-Length: "...)
	buf = strconv.AppendInt(buf, int64(len(body)), 10)
	buf = append(buf, "\r\nConnection: close\r\n\r\n"...)
	buf = append(buf, body...)

	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("write OTLP request: %w", err)
	}

	// Read just the status line to check for errors.
	var resp [128]byte
	n, _ := conn.Read(resp[:])
	if n < 12 {
		return fmt.Errorf("OTLP response too short")
	}
	// "HTTP/1.1 200" — status code starts at byte 9
	if resp[9] >= '4' {
		return fmt.Errorf("OTLP export failed: %s", string(resp[:n]))
	}
	return nil
}

// OTLPEndpoint holds the parsed host:port and path for an OTLP endpoint.
type OTLPEndpoint struct {
	addr string // host:port
	path string // e.g. "/v1/traces"
}

// ParseOTLPEndpoint parses OTEL_EXPORTER_OTLP_ENDPOINT into addr and path.
// Returns nil if the env var is unset.
func ParseOTLPEndpoint() *OTLPEndpoint {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		// Treat as host:port directly.
		return &OTLPEndpoint{addr: endpoint, path: "/v1/traces"}
	}
	addr := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	return &OTLPEndpoint{addr: addr, path: "/v1/traces"}
}

// Init sets up the global span sink with an OTLP HTTP flusher.
// Returns a shutdown function. If OTEL_EXPORTER_OTLP_ENDPOINT is unset,
// tracing is disabled and the returned shutdown is a no-op.
func Init(ctx context.Context, serviceName string) func(context.Context) error {
	ep := ParseOTLPEndpoint()
	if ep == nil {
		return func(context.Context) error { return nil }
	}
	f := NewFlusher(ctx, ep.addr, ep.path, serviceName, 100*time.Millisecond)
	SetSink(f)
	log.G(ctx).WithField("endpoint", ep.addr).Debug("tracing enabled")
	return func(ctx context.Context) error {
		SetSink(nil)
		return f.Shutdown(ctx)
	}
}
