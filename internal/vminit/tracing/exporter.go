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
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	tracespb "github.com/containerd/nerdbox/api/services/traces/v1"
)

const channelCapacity = 1024

// Exporter is an OTel SpanExporter that buffers spans in a channel for
// streaming over ttrpc. Spans are silently dropped if the channel is full
// to ensure tracing never blocks the container lifecycle.
type Exporter struct {
	ch        chan *tracespb.Span
	done      chan struct{}
	closeOnce sync.Once
}

// NewExporter creates a new channel-backed span exporter.
func NewExporter() *Exporter {
	return &Exporter{
		ch:   make(chan *tracespb.Span, channelCapacity),
		done: make(chan struct{}),
	}
}

// ExportSpans converts ReadOnlySpans to proto and sends them on the channel.
func (e *Exporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		ps := spanToProto(s)
		select {
		case <-e.done:
			return nil
		case e.ch <- ps:
		default:
			// Drop span — tracing must never block.
		}
	}
	return nil
}

// Shutdown signals writers to stop. The channel is not closed to avoid
// panics from concurrent sends; readers should select on Done() instead.
func (e *Exporter) Shutdown(_ context.Context) error {
	e.closeOnce.Do(func() {
		close(e.done)
	})
	return nil
}

// Chan returns the span channel for reading by the ttrpc service.
func (e *Exporter) Chan() <-chan *tracespb.Span {
	return e.ch
}

// Done returns a channel that is closed when the exporter is shut down.
func (e *Exporter) Done() <-chan struct{} {
	return e.done
}

func spanToProto(s sdktrace.ReadOnlySpan) *tracespb.Span {
	sc := s.SpanContext()
	tid := sc.TraceID()
	sid := sc.SpanID()
	psc := s.Parent()
	psid := psc.SpanID()

	var attrs []*tracespb.KeyValue
	for _, kv := range s.Attributes() {
		attrs = append(attrs, &tracespb.KeyValue{
			Key:   string(kv.Key),
			Value: kv.Value.Emit(),
		})
	}

	return &tracespb.Span{
		TraceID:           tid[:],
		SpanID:            sid[:],
		ParentSpanID:      psid[:],
		Name:              s.Name(),
		StartTimeUnixNano: s.StartTime().UnixNano(),
		EndTimeUnixNano:   s.EndTime().UnixNano(),
		Kind:              int32(s.SpanKind()),
		StatusCode:        int32(s.Status().Code),
		StatusMessage:     s.Status().Description,
		Attributes:        attrs,
	}
}

// Verify interface compliance.
var _ sdktrace.SpanExporter = (*Exporter)(nil)
