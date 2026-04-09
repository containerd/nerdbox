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
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/containerd/log"

	tracespb "github.com/containerd/nerdbox/api/services/traces/v1"
)

// ForwardTraces reads spans from the VM trace stream and exports them
// to the OTLP endpoint as JSON. hostBootTime is the host wall-clock time
// captured when ttrpc became responsive, used to correct VM-vs-host clock skew.
func ForwardTraces(ctx context.Context, stream tracespb.TTRPCTraces_StreamClient, endpoint string, hostBootTime time.Time) {
	client := &http.Client{}

	// The VM's RTC has only second-level resolution, so its wall clock
	// can be up to ~1s behind the host. We compute the offset from the
	// first interceptor span (which is created at the moment the first
	// ttrpc RPC reaches the VM — a known sync point with the host).
	// hostBootTime was captured on the host at the same logical moment
	// (when ttrpc became responsive).
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

		if err := exportVMSpan(ctx, client, endpoint, span, clockOffset); err != nil {
			log.G(ctx).WithError(err).Warn("trace relay export")
		}
	}
}

func exportVMSpan(ctx context.Context, client *http.Client, endpoint string, s *tracespb.Span, clockOffset time.Duration) error {
	startNano := time.Unix(0, s.StartTimeUnixNano).Add(clockOffset).UnixNano()
	endNano := time.Unix(0, s.EndTimeUnixNano).Add(clockOffset).UnixNano()

	span := otlpSpan{
		TraceID:           hex.EncodeToString(s.TraceID),
		SpanID:            hex.EncodeToString(s.SpanID),
		ParentSpanID:      hex.EncodeToString(s.ParentSpanID),
		Name:              s.Name,
		Kind:              int(s.Kind),
		StartTimeUnixNano: strconv.FormatInt(startNano, 10),
		EndTimeUnixNano:   strconv.FormatInt(endNano, 10),
		Status: otlpStatus{
			Code:    int(s.StatusCode),
			Message: s.StatusMessage,
		},
	}

	for _, kv := range s.Attributes {
		span.Attributes = append(span.Attributes, otlpKeyValue{
			Key:   kv.Key,
			Value: otlpAnyValue{StringValue: kv.Value},
		})
	}

	req := otlpExportRequest{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{
				Attributes: []otlpKeyValue{{
					Key:   "service.name",
					Value: otlpAnyValue{StringValue: "nerdbox-vm"},
				}},
			},
			ScopeSpans: []otlpScopeSpans{{
				Spans: []otlpSpan{span},
			}},
		}},
	}

	return postOTLP(ctx, client, endpoint, req)
}
