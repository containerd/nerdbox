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

package shimtest

import (
	"context"
	"sync"
	"testing"
	"time"

	eventsapi "github.com/containerd/containerd/api/services/ttrpc/events/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// eventRecorders caches the recorder per bundleDir so that
// startEventsRecorder is idempotent: shimSetup binds one preemptively
// (so the shim's event publishes never hang on a missing pipe server),
// and tests that need a reference for assertions get the same instance
// instead of trying to create a second listener on the same path.
var eventRecorders sync.Map // map[string]*eventRecorder

// eventRecorder implements the containerd ttrpc events service and
// records every envelope it receives. Shims push events to this
// endpoint (the TTRPC_ADDRESS the shim reads from its environment);
// tests can then assert on topic order, contents, and timing.
type eventRecorder struct {
	mu        sync.Mutex
	envelopes []*types.Envelope
}

// Forward implements the eventsapi.TTRPCEventsService.Forward method.
func (r *eventRecorder) Forward(_ context.Context, req *eventsapi.ForwardRequest) (*emptypb.Empty, error) {
	r.mu.Lock()
	r.envelopes = append(r.envelopes, req.Envelope)
	r.mu.Unlock()
	return &emptypb.Empty{}, nil
}

// topics returns the ordered topic strings recorded so far.
func (r *eventRecorder) topics() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.envelopes))
	for i, e := range r.envelopes {
		out[i] = e.Topic
	}
	return out
}

// waitForTopic polls until an envelope with the given topic is
// recorded, or the timeout expires. Returns nil on timeout.
func (r *eventRecorder) waitForTopic(topic string, timeout time.Duration) *types.Envelope {
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		for _, e := range r.envelopes {
			if e.Topic == topic {
				r.mu.Unlock()
				return e
			}
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startEventsRecorder binds a TTRPC events server to the socket path
// the shim dials as its events endpoint and returns a recorder
// capturing every forwarded envelope. Must be called before startShim
// so the shim's first publish succeeds.
//
// Idempotent per bundleDir: if a recorder has already been started for
// this bundleDir (e.g. by shimSetup), returns the existing instance
// instead of trying to bind a second listener on the same address —
// which would fail with EADDRINUSE on Unix and ACCESS_DENIED on Windows.
func startEventsRecorder(tb testing.TB, bundleDir string) *eventRecorder {
	tb.Helper()

	if v, ok := eventRecorders.Load(bundleDir); ok {
		return v.(*eventRecorder)
	}

	socketPath := containerdSockPath(tb, bundleDir)

	// listenEvents is platform-specific (connect_unix.go / connect_windows.go):
	// Unix uses net.Listen("unix",...); Windows uses winio.ListenPipe.
	ln := listenEvents(tb, socketPath)

	srv, err := ttrpc.NewServer()
	if err != nil {
		ln.Close()
		tb.Fatal("events server:", err)
	}

	rec := &eventRecorder{}
	eventsapi.RegisterTTRPCEventsService(srv, rec)

	eventRecorders.Store(bundleDir, rec)

	go srv.Serve(context.Background(), ln)

	tb.Cleanup(func() {
		eventRecorders.Delete(bundleDir)
		srv.Close()
		ln.Close()
	})

	return rec
}
