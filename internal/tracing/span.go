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

// Package tracing provides minimal distributed tracing with OTLP export.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// TraceID is a 16-byte trace identifier.
type TraceID [16]byte

func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// SpanID is an 8-byte span identifier.
type SpanID [8]byte

func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

// Span represents a named time interval in a trace.
type Span struct {
	TraceID      TraceID
	SpanID       SpanID
	ParentSpanID SpanID
	Name         string
	StartTime    time.Time
	EndTime      time.Time
	ended        atomic.Bool
}

// End records the span's end time. Idempotent — second calls are no-ops.
// Finished spans are sent to the global sink (if configured).
func (s *Span) End() {
	if s == nil || !s.ended.CompareAndSwap(false, true) {
		return
	}
	s.EndTime = time.Now()
	if sink := globalSink.Load(); sink != nil {
		(*sink).Collect(s)
	}
}

type contextKey struct{}

// Start creates a child span of the span in ctx (if any), stores it in the
// returned context, and returns it. The caller must call span.End().
func Start(ctx context.Context, name string) (context.Context, *Span) {
	span := &Span{
		Name:      name,
		StartTime: time.Now(),
		SpanID:    newSpanID(),
	}
	if parent := FromContext(ctx); parent != nil {
		span.TraceID = parent.TraceID
		span.ParentSpanID = parent.SpanID
	} else if remote := remoteSpanFromContext(ctx); remote != nil {
		span.TraceID = remote.TraceID
		span.ParentSpanID = remote.SpanID
	} else {
		span.TraceID = newTraceID()
	}
	return context.WithValue(ctx, contextKey{}, span), span
}

// FromContext returns the current Span from ctx, or nil.
func FromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(contextKey{}).(*Span)
	return s
}

// remoteSpanContext holds trace/span IDs extracted from an incoming ttrpc call.
type remoteSpanContext struct {
	TraceID TraceID
	SpanID  SpanID
}

type remoteContextKey struct{}

func withRemoteSpan(ctx context.Context, r *remoteSpanContext) context.Context {
	return context.WithValue(ctx, remoteContextKey{}, r)
}

func remoteSpanFromContext(ctx context.Context) *remoteSpanContext {
	r, _ := ctx.Value(remoteContextKey{}).(*remoteSpanContext)
	return r
}

// Sink receives finished spans.
type Sink interface {
	Collect(s *Span)
}

var globalSink atomic.Pointer[Sink]

// SetSink sets the global span sink. Call with nil to disable collection.
func SetSink(s Sink) {
	if s != nil {
		globalSink.Store(&s)
	} else {
		globalSink.Store(nil)
	}
}

var (
	randMu sync.Mutex
)

func newTraceID() TraceID {
	var id TraceID
	randMu.Lock()
	rand.Read(id[:])
	randMu.Unlock()
	return id
}

func newSpanID() SpanID {
	var id SpanID
	randMu.Lock()
	rand.Read(id[:])
	randMu.Unlock()
	return id
}

// ParseTraceID parses a 32-char hex string into a TraceID.
func ParseTraceID(s string) (TraceID, error) {
	var id TraceID
	if len(s) != 32 {
		return id, fmt.Errorf("invalid trace ID length: %d", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	copy(id[:], b)
	return id, nil
}

// ParseSpanID parses a 16-char hex string into a SpanID.
func ParseSpanID(s string) (SpanID, error) {
	var id SpanID
	if len(s) != 16 {
		return id, fmt.Errorf("invalid span ID length: %d", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	copy(id[:], b)
	return id, nil
}
