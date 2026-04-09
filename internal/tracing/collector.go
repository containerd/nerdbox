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
	"sync"

	tracespb "github.com/containerd/nerdbox/api/services/traces/v1"
)

const collectorCapacity = 1024

// Collector buffers finished spans in a channel for streaming over ttrpc.
// Spans are silently dropped if the channel is full.
type Collector struct {
	ch        chan *tracespb.Span
	done      chan struct{}
	closeOnce sync.Once
}

// NewCollector creates a new channel-backed span collector.
func NewCollector() *Collector {
	return &Collector{
		ch:   make(chan *tracespb.Span, collectorCapacity),
		done: make(chan struct{}),
	}
}

// Collect converts a finished Span to proto and buffers it.
func (c *Collector) Collect(s *Span) {
	ps := &tracespb.Span{
		TraceID:           s.TraceID[:],
		SpanID:            s.SpanID[:],
		ParentSpanID:      s.ParentSpanID[:],
		Name:              s.Name,
		StartTimeUnixNano: s.StartTime.UnixNano(),
		EndTimeUnixNano:   s.EndTime.UnixNano(),
		Kind:              1, // INTERNAL
		StatusCode:        0, // UNSET
	}
	select {
	case <-c.done:
	case c.ch <- ps:
	default:
		// Drop span — tracing must never block.
	}
}

// Shutdown signals the collector to stop.
func (c *Collector) Shutdown() {
	c.closeOnce.Do(func() {
		close(c.done)
	})
}

// Chan returns the span channel for reading by the ttrpc service.
func (c *Collector) Chan() <-chan *tracespb.Span {
	return c.ch
}

// Done returns a channel that is closed when the collector is shut down.
func (c *Collector) Done() <-chan struct{} {
	return c.done
}
