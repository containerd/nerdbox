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

package streaming

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	streamapi "github.com/containerd/containerd/api/services/streaming/v1"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
)

// fakeSandbox is a minimal sandbox.Sandbox that hands out one side of a
// net.Pipe as the "VM" connection. The other side is kept so the test
// can drive it (write data, close, etc.).
type fakeSandbox struct {
	mu      sync.Mutex
	vmSides []net.Conn
}

func (s *fakeSandbox) Start(context.Context, ...sandbox.Opt) error { return nil }
func (s *fakeSandbox) Stop(context.Context) error                  { return nil }
func (s *fakeSandbox) Client() (*ttrpc.Client, error)              { return nil, nil }

func (s *fakeSandbox) StartStream(_ context.Context, _ string) (net.Conn, error) {
	host, vm := net.Pipe()
	s.mu.Lock()
	s.vmSides = append(s.vmSides, vm)
	s.mu.Unlock()
	return host, nil
}

func (s *fakeSandbox) VMSides() []net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]net.Conn, len(s.vmSides))
	copy(out, s.vmSides)
	return out
}

// newTTRPCPair spins up a ttrpc.Server and a ttrpc.Client connected by a
// net.Pipe so tests can drive the shim's service code end-to-end.
func newTTRPCPair(t *testing.T) (srv *ttrpc.Server, cli *ttrpc.Client, cleanup func()) {
	t.Helper()
	serverConn, clientConn := net.Pipe()

	s, err := ttrpc.NewServer()
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	l := &onceListener{conn: serverConn, done: make(chan struct{})}
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- s.Serve(context.Background(), l)
	}()

	c := ttrpc.NewClient(clientConn)

	return s, c, func() {
		t.Helper()
		c.Close()
		s.Close()
		close(l.done)
		if err := <-serveErrCh; err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, ttrpc.ErrServerClosed) {
			t.Errorf("ttrpc server exited unexpectedly: %v", err)
		}
	}
}

type onceListener struct {
	conn   net.Conn
	served bool
	mu     sync.Mutex
	done   chan struct{}
}

func (l *onceListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.served {
		l.mu.Unlock()
		<-l.done
		return nil, net.ErrClosed
	}
	l.served = true
	c := l.conn
	l.mu.Unlock()
	return c, nil
}
func (l *onceListener) Close() error   { return l.conn.Close() }
func (l *onceListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// TestStreamReturnsOnVMClose covers the core of issue #701: when the VM
// closes its side of the data channel (either because vminitd finished
// sending data or because it is about to return an error), the shim's
// Stream handler must return so the TTRPC server stream closes. Without
// that close, the daemon's ReceiveStream loop never sees EOF on its
// stream.Recv, it keeps the stream alive, and follow-up RPCs observe
// delays consistent with the 6-minute test-framework timeout.
//
// The scenario: open a Stream, do the StreamInit handshake, have the
// "VM" side close its end, and verify the server-side stream closes
// within a short bound. Before the fix this hangs until the enclosing
// context is cancelled.
func TestStreamReturnsOnVMClose(t *testing.T) {
	srv, cli, cleanup := newTTRPCPair(t)
	defer cleanup()

	fb := &fakeSandbox{}
	svc := &service{sb: fb}
	if err := svc.RegisterTTRPC(srv); err != nil {
		t.Fatalf("RegisterTTRPC: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sc := streamapi.NewTTRPCStreamingClient(cli)
	stream, err := sc.Stream(ctx)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	initMsg, err := typeurl.MarshalAnyToProto(&streamapi.StreamInit{ID: "archive-write-test"})
	if err != nil {
		t.Fatalf("MarshalAny StreamInit: %v", err)
	}
	if err := stream.Send(initMsg); err != nil {
		t.Fatalf("Send StreamInit: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv ack: %v", err)
	}

	// VM closes its side of the data channel. This is what vminitd does
	// after finishing a WriteStream (e.g. after writing a tar archive
	// for a successful stat) — it calls stream.Close on the data
	// channel via defer in the transfer service handler.
	vmSides := fb.VMSides()
	if len(vmSides) != 1 {
		t.Fatalf("expected 1 VM side, got %d", len(vmSides))
	}
	vmSides[0].Close()

	// The shim's Stream handler should now return, and that should close
	// the TTRPC server stream — surfacing as io.EOF on the next Recv.
	// Guard with a tight timeout so a regression (the handler blocking
	// on bridgeTTRPCToVM) shows up as a test timeout rather than hanging
	// the whole suite.
	recvDone := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		recvDone <- err
	}()

	select {
	case err := <-recvDone:
		if err == nil {
			t.Fatal("expected io.EOF or similar, got nil")
		}
		if !errors.Is(err, io.EOF) && err.Error() != "EOF" {
			// Different ttrpc builds may surface the close as either
			// io.EOF or a typed EOF status; accept both.
			t.Logf("stream closed with: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shim Stream handler did not release the TTRPC stream after VM closed its side — #701 regression")
	}
}

// TestMultipleStreamsDoNotAccumulate is a stress test: open a handful of
// streams, have the VM close each one, and verify that the shim promptly
// releases them all. Before the fix, each closed stream leaves the shim
// Stream handler running, and resources accumulate.
func TestMultipleStreamsDoNotAccumulate(t *testing.T) {
	srv, cli, cleanup := newTTRPCPair(t)
	defer cleanup()

	fb := &fakeSandbox{}
	svc := &service{sb: fb}
	if err := svc.RegisterTTRPC(srv); err != nil {
		t.Fatalf("RegisterTTRPC: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sc := streamapi.NewTTRPCStreamingClient(cli)

	const nStreams = 5
	for i := 0; i < nStreams; i++ {
		stream, err := sc.Stream(ctx)
		if err != nil {
			t.Fatalf("Stream %d: %v", i, err)
		}
		initMsg, err := typeurl.MarshalAnyToProto(&streamapi.StreamInit{ID: "s"})
		if err != nil {
			t.Fatalf("MarshalAny StreamInit %d: %v", i, err)
		}
		if err := stream.Send(initMsg); err != nil {
			t.Fatalf("Send StreamInit %d: %v", i, err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("Recv ack %d: %v", i, err)
		}

		// Close the VM side; the shim must release this stream promptly.
		vmSides := fb.VMSides()
		vmSides[i].Close()

		recvDone := make(chan error, 1)
		go func() {
			_, err := stream.Recv()
			recvDone <- err
		}()
		select {
		case <-recvDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("stream %d not released within 2s — #701 regression", i)
		}
	}
}
