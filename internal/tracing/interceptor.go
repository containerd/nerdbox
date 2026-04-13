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
	"fmt"
	"strings"

	"github.com/containerd/ttrpc"
)

const traceparentKey = "traceparent"

// UnaryClientInterceptor returns a ttrpc.UnaryClientInterceptor that
// creates a client span and injects traceparent into outgoing metadata.
func UnaryClientInterceptor() ttrpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		req *ttrpc.Request, resp *ttrpc.Response,
		info *ttrpc.UnaryClientInfo,
		invoker ttrpc.Invoker,
	) error {
		ctx, span := Start(ctx, info.FullMethod)
		defer span.End()

		// Inject traceparent into ttrpc request metadata.
		tp := fmt.Sprintf("00-%s-%s-01", span.TraceID, span.SpanID)
		req.Metadata = append(req.Metadata, &ttrpc.KeyValue{
			Key:   traceparentKey,
			Value: tp,
		})

		return invoker(ctx, req, resp)
	}
}

// UnaryServerInterceptor returns a ttrpc.UnaryServerInterceptor that
// extracts traceparent from incoming metadata and creates a server span.
func UnaryServerInterceptor() ttrpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		unmarshal ttrpc.Unmarshaler,
		info *ttrpc.UnaryServerInfo,
		method ttrpc.Method,
	) (interface{}, error) {
		// Extract traceparent from ttrpc metadata.
		ctx = extractTraceparent(ctx)

		ctx, span := Start(ctx, info.FullMethod)
		defer span.End()

		return method(ctx, unmarshal)
	}
}

// extractTraceparent reads the traceparent header from ttrpc metadata
// and stores the remote span context in ctx.
func extractTraceparent(ctx context.Context) context.Context {
	md, ok := ttrpc.GetMetadata(ctx)
	if !ok {
		return ctx
	}
	values, ok := md.Get(traceparentKey)
	if !ok || len(values) == 0 {
		return ctx
	}
	tp := values[0]

	// Parse: "00-{traceID}-{spanID}-{flags}"
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		return ctx
	}
	traceID, err := ParseTraceID(parts[1])
	if err != nil {
		return ctx
	}
	spanID, err := ParseSpanID(parts[2])
	if err != nil {
		return ctx
	}

	return withRemoteSpan(ctx, &remoteSpanContext{
		TraceID: traceID,
		SpanID:  spanID,
	})
}
