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
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/containerd/log"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	tracespb "github.com/containerd/nerdbox/api/services/traces/v1"
)

// ForwardTraces reads spans from the VM trace stream and exports them
// to the OTLP endpoint. hostBootTime is the host wall-clock time captured
// when ttrpc became responsive, used to correct VM-vs-host clock skew.
func ForwardTraces(ctx context.Context, stream tracespb.TTRPCTraces_StreamClient, endpoint string, hostBootTime time.Time) {
	client := &http.Client{}

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

		if err := exportSpan(ctx, client, endpoint, span, clockOffset); err != nil {
			log.G(ctx).WithError(err).Warn("trace relay export")
		}
	}
}

func exportSpan(ctx context.Context, client *http.Client, endpoint string, s *tracespb.Span, clockOffset time.Duration) error {
	startNano := time.Unix(0, s.StartTimeUnixNano).Add(clockOffset).UnixNano()
	endNano := time.Unix(0, s.EndTimeUnixNano).Add(clockOffset).UnixNano()

	span := &tracepb.Span{
		TraceId:           s.TraceID,
		SpanId:            s.SpanID,
		ParentSpanId:      s.ParentSpanID,
		Name:              s.Name,
		Kind:              tracepb.Span_SpanKind(s.Kind),
		StartTimeUnixNano: uint64(startNano),
		EndTimeUnixNano:   uint64(endNano),
		Status: &tracepb.Status{
			Code:    tracepb.Status_StatusCode(s.StatusCode),
			Message: s.StatusMessage,
		},
	}

	for _, kv := range s.Attributes {
		span.Attributes = append(span.Attributes, &commonpb.KeyValue{
			Key:   kv.Key,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: kv.Value}},
		})
	}

	req := &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{{
					Key:   "service.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "nerdbox-vm"}},
				}},
			},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{span},
			}},
		}},
	}

	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal OTLP request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send OTLP request: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("OTLP export failed: %s", resp.Status)
	}
	return nil
}
