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
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	streamapi "github.com/containerd/containerd/api/services/streaming/v1"
	"github.com/containerd/errdefs"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
)

// fakeStreamServer implements streamapi.TTRPCStreaming_StreamServer in
// memory so the service can be invoked without spinning up a ttrpc
// server. The test feeds inbound messages via recvCh and reads outbound
// ones via sendCh.
type fakeStreamServer struct {
	ctx    context.Context
	recvCh chan *anypb.Any
	sendCh chan *anypb.Any

	mu     sync.Mutex
	closed bool
}

func newFakeStreamServer(ctx context.Context) *fakeStreamServer {
	return &fakeStreamServer{
		ctx:    ctx,
		recvCh: make(chan *anypb.Any, 16),
		sendCh: make(chan *anypb.Any, 16),
	}
}

// closeRecv signals end-of-stream to the handler's Recv loop, mirroring
// a client that has called CloseSend.
func (f *fakeStreamServer) closeRecv() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.recvCh)
}

func (f *fakeStreamServer) Send(m *anypb.Any) error {
	select {
	case f.sendCh <- m:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

func (f *fakeStreamServer) Recv() (*anypb.Any, error) {
	select {
	case a, ok := <-f.recvCh:
		if !ok {
			return nil, io.EOF
		}
		return a, nil
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

// SendMsg implements ttrpc.StreamServer. It is only invoked when a
// caller goes through the generic ttrpc.StreamServer interface; the
// streaming wrapper always passes *anypb.Any here. We assert that
// explicitly and return a clear error on misuse instead of panicking.
func (f *fakeStreamServer) SendMsg(m interface{}) error {
	a, ok := m.(*anypb.Any)
	if !ok {
		return fmt.Errorf("fakeStreamServer.SendMsg: expected *anypb.Any, got %T", m)
	}
	return f.Send(a)
}

// RecvMsg implements ttrpc.StreamServer. The streaming wrapper always
// passes a freshly-allocated *anypb.Any; we copy fields into it
// directly rather than going through proto.Merge, which would silently
// misbehave on a type mismatch.
func (f *fakeStreamServer) RecvMsg(m interface{}) error {
	dst, ok := m.(*anypb.Any)
	if !ok {
		return fmt.Errorf("fakeStreamServer.RecvMsg: expected *anypb.Any, got %T", m)
	}
	a, err := f.Recv()
	if err != nil {
		return err
	}
	dst.TypeUrl = a.TypeUrl
	dst.Value = a.Value
	return nil
}

// fakeSandbox is a minimal sandbox.Sandbox that hands out a pre-supplied
// net.Conn from StartStream. The other methods are not exercised by the
// Stream handler.
type fakeSandbox struct {
	conn net.Conn
}

func (s *fakeSandbox) Start(context.Context, ...sandbox.Opt) error { return errdefs.ErrNotImplemented }
func (s *fakeSandbox) Stop(context.Context) error                  { return errdefs.ErrNotImplemented }
func (s *fakeSandbox) Client() (*ttrpc.Client, error)              { return nil, errdefs.ErrNotImplemented }
func (s *fakeSandbox) StartStream(context.Context, string) (net.Conn, error) {
	return s.conn, nil
}

// streamInitAny marshals StreamInit{ID: id} as an *anypb.Any so it can
// be fed to the handler through the fake server's Recv channel.
func streamInitAny(t *testing.T, id string) *anypb.Any {
	t.Helper()
	a, err := typeurl.MarshalAnyToProto(&streamapi.StreamInit{ID: id})
	if err != nil {
		t.Fatalf("marshal StreamInit: %v", err)
	}
	return a
}

// startStream wires up a service+fake harness, kicks off the Stream
// handler in a goroutine, drains the post-init ack, and returns the
// pieces a test needs to drive the bridge.
func startStream(t *testing.T, ctx context.Context, id string) (srv *fakeStreamServer, vmSide net.Conn, done <-chan error) {
	t.Helper()

	shimSide, vm := net.Pipe()
	t.Cleanup(func() {
		shimSide.Close()
		vm.Close()
	})

	srv = newFakeStreamServer(ctx)
	srv.recvCh <- streamInitAny(t, id)

	svc := &service{sb: &fakeSandbox{conn: shimSide}, streams: make(map[string]net.Conn)}

	d := make(chan error, 1)
	go func() { d <- svc.Stream(ctx, srv) }()

	// Drain the ack the service sends right after StreamInit.
	select {
	case <-srv.sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream init ack")
	}
	return srv, vm, d
}

// TestStreamReturnsAfterVMEOFWithoutClientClose reproduces the deadlock
// fixed by the surrounding change. In a unidirectional VM->client
// transfer the client never issues CloseSend, so bridgeTTRPCToVM stays
// blocked in srv.Recv() forever. When the VM signals end-of-stream with
// a zero-length frame the handler must still return promptly so ttrpc
// closes the server stream and the client unblocks; without the fix the
// handler waits for both bridge directions and hangs indefinitely.
func TestStreamReturnsAfterVMEOFWithoutClientClose(t *testing.T) {
	ctx := t.Context()

	_, vmSide, done := startStream(t, ctx, "stream-eof")

	// VM finishes work without sending any data and signals EOF with a
	// zero-length frame. The fake server is intentionally left with
	// nothing more to deliver via Recv, so bridgeTTRPCToVM remains
	// blocked just like a real handler waiting on a quiet client.
	if err := binary.Write(vmSide, binary.BigEndian, uint32(0)); err != nil {
		t.Fatalf("write VM EOF marker: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stream returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream handler did not return after VM EOF; the client->server bridge is still blocked in srv.Recv() and the handler is waiting for both directions to finish")
	}
}

// TestStreamReturnsWhenVMEOFAfterClientClose covers the case where the
// client sends CloseSend first and the VM finishes shortly after. The
// handler must still wait for the VM->client direction to drain before
// returning so no in-flight server replies are lost.
func TestStreamReturnsWhenVMEOFAfterClientClose(t *testing.T) {
	ctx := t.Context()

	srv, vmSide, done := startStream(t, ctx, "stream-client-close")

	// Client closes its send side. bridgeTTRPCToVM observes io.EOF and
	// writes the zero-length frame to the VM.
	srv.closeRecv()

	// Drain the EOF marker that bridgeTTRPCToVM forwards to the VM so
	// the pipe write does not block.
	go func() {
		var n uint32
		_ = binary.Read(vmSide, binary.BigEndian, &n)
	}()

	// Handler must NOT have returned yet — the VM->client direction is
	// still open. Give it a brief moment to settle and confirm it is
	// still running.
	select {
	case err := <-done:
		t.Fatalf("Stream returned before VM EOF (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}

	// VM signals end-of-stream; handler returns.
	if err := binary.Write(vmSide, binary.BigEndian, uint32(0)); err != nil {
		t.Fatalf("write VM EOF marker: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stream returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream handler did not return after VM EOF")
	}
}

// TestStreamForwardsBothDirections is a sanity check that data still
// moves through the bridge correctly so the regression coverage above
// is not vacuous.
func TestStreamForwardsBothDirections(t *testing.T) {
	ctx := t.Context()

	srv, vmSide, done := startStream(t, ctx, "stream-bidi")

	// Client -> VM: enqueue payloads through the fake server's recv
	// channel and confirm the VM peer reads them as length-prefixed
	// proto Any frames.
	payloads := [][]byte{[]byte("hello"), []byte("world")}
	for _, p := range payloads {
		srv.recvCh <- &anypb.Any{TypeUrl: "test/bytes", Value: p}
	}
	for i, want := range payloads {
		var n uint32
		if err := binary.Read(vmSide, binary.BigEndian, &n); err != nil {
			t.Fatalf("read frame %d length: %v", i, err)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(vmSide, buf); err != nil {
			t.Fatalf("read frame %d data: %v", i, err)
		}
		var got anypb.Any
		if err := proto.Unmarshal(buf, &got); err != nil {
			t.Fatalf("unmarshal frame %d: %v", i, err)
		}
		if !bytes.Equal(got.Value, want) {
			t.Fatalf("VM frame %d = %q, want %q", i, got.Value, want)
		}
	}

	// VM -> client: write framed messages and verify the client picks
	// them up via Send.
	replies := []string{"reply-1", "reply-2"}
	for _, p := range replies {
		frame := &anypb.Any{TypeUrl: "test/bytes", Value: []byte(p)}
		data, err := proto.Marshal(frame)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := binary.Write(vmSide, binary.BigEndian, uint32(len(data))); err != nil {
			t.Fatalf("write frame length: %v", err)
		}
		if _, err := vmSide.Write(data); err != nil {
			t.Fatalf("write frame data: %v", err)
		}
	}
	for _, want := range replies {
		select {
		case got := <-srv.sendCh:
			if string(got.Value) != want {
				t.Fatalf("client received %q, want %q", got.Value, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q from server", want)
		}
	}

	// VM signals EOF; handler must return without error.
	if err := binary.Write(vmSide, binary.BigEndian, uint32(0)); err != nil {
		t.Fatalf("write VM EOF: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stream returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream handler did not return after VM EOF")
	}
}

// TestStreamReturnsAfterContextCancelOnIdleStream covers the abandoned-
// exec scenario: a client disconnects (per-RPC ctx cancels) while the
// VM holds the stream open but is not sending any data. The handler
// must drain via the SetReadDeadline-driven exit path on <-ctx.Done()
// rather than leak its goroutines and the vmConn FD waiting for a VM
// EOF that will never come.
//
// The c2v goroutine exits cleanly when srv.Recv() observes ctx.Err(),
// but its zero-length EOF marker is written into the shim->VM half of
// the pipe; v2t reads from the VM->shim half and therefore never sees
// it. The handler's <-ctx.Done() branch must SetReadDeadline on vmConn
// to unblock v2t's binary.Read.
func TestStreamReturnsAfterContextCancelOnIdleStream(t *testing.T) {
	parent := t.Context()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	_, _, done := startStream(t, ctx, "stream-cancel-idle")

	// Neither side ever sends a frame. The handler is now waiting on
	// v2t. Cancelling the per-RPC ctx mimics ttrpc tearing down the
	// stream after the caller has gone away.
	cancel()

	select {
	case err := <-done:
		// Either nil (clean drain) or context.Canceled (propagated
		// from srv.Recv) is acceptable; what matters is that the
		// handler returned at all.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Stream returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream handler did not return after ctx cancel; v2t is still blocked in binary.Read(vmConn). The fix must give ctx.Done() a controlled exit path that unblocks the VM->client bridge (e.g. SetReadDeadline) without firing vmConn.Close() before v2t has drained.")
	}
}

// blockingStartSandbox blocks in StartStream until ctx is cancelled or
// release is closed, mimicking the real libkrun StartStream retry loop
// that polls for the guest stream socket to appear and observes
// ctx.Done() between attempts.
type blockingStartSandbox struct {
	called  chan struct{}
	release chan struct{}
}

func (s *blockingStartSandbox) Start(context.Context, ...sandbox.Opt) error {
	return errdefs.ErrNotImplemented
}
func (s *blockingStartSandbox) Stop(context.Context) error     { return errdefs.ErrNotImplemented }
func (s *blockingStartSandbox) Client() (*ttrpc.Client, error) { return nil, errdefs.ErrNotImplemented }
func (s *blockingStartSandbox) StartStream(ctx context.Context, _ string) (net.Conn, error) {
	close(s.called)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return nil, errdefs.ErrNotImplemented
	}
}

// TestStreamPreservesStartStreamCancel asserts that StartStream observes
// the caller's ctx cancellation: a timed-out or cancelled RPC must abort
// connection establishment promptly rather than block in libkrun's
// retry loop for the full polling window.
func TestStreamPreservesStartStreamCancel(t *testing.T) {
	parent := t.Context()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sb := &blockingStartSandbox{
		called:  make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() { close(sb.release) })

	srv := newFakeStreamServer(ctx)
	srv.recvCh <- streamInitAny(t, "slow-start")

	svc := &service{sb: sb, streams: make(map[string]net.Conn)}

	done := make(chan error, 1)
	go func() { done <- svc.Stream(ctx, srv) }()

	select {
	case <-sb.called:
	case <-time.After(2 * time.Second):
		t.Fatal("StartStream was never invoked")
	}

	// Caller is gone. StartStream must observe and abort.
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled propagation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return after ctx cancel during StartStream; the retry loop ignored the cancel signal. The handler must pass the caller's ctx to StartStream so its retry loop aborts on cancel/timeout.")
	}
}

// TestShutdownDrainsAllInFlightStreams covers the shim shutdown drain
// path: when ContainerStop tears down the kit shim, multiple Stream
// handlers may be in flight simultaneously (dockerd healthcheck poll,
// readiness exec, sentinel hold, ...) and their bridges race the
// transport teardown. The shutdown callback must SetReadDeadline on
// every tracked vmConn and wait for all handlers to exit before
// returning, so that vmConn.Close() has fired on every stream by the
// time sandbox.Stop tears down the VM.
func TestShutdownDrainsAllInFlightStreams(t *testing.T) {
	ctx := t.Context()

	const n = 3
	shimSides := make([]net.Conn, n)
	vmSides := make([]net.Conn, n)
	conns := make(map[string]net.Conn, n)
	for i := 0; i < n; i++ {
		shim, vm := net.Pipe()
		shimSides[i] = shim
		vmSides[i] = vm
		conns[fmt.Sprintf("stream-%d", i)] = shim
		t.Cleanup(func() { shim.Close(); vm.Close() })
	}

	svc := &service{
		sb:      &fakeMultiSandbox{conns: conns},
		streams: make(map[string]net.Conn),
	}

	// Open n concurrent streams; none send any data so all handlers
	// are blocked in v2t's binary.Read.
	dones := make([]<-chan error, n)
	srvs := make([]*fakeStreamServer, n)
	for i := 0; i < n; i++ {
		srvs[i] = newFakeStreamServer(ctx)
		srvs[i].recvCh <- streamInitAny(t, fmt.Sprintf("stream-%d", i))
		d := make(chan error, 1)
		go func(i int) { d <- svc.Stream(ctx, srvs[i]) }(i)
		dones[i] = d
		select {
		case <-srvs[i].sendCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("stream %d: timed out waiting for ack", i)
		}
	}

	// Confirm all n streams are tracked.
	svc.mu.Lock()
	if len(svc.streams) != n {
		svc.mu.Unlock()
		t.Fatalf("expected %d tracked streams, got %d", n, len(svc.streams))
	}
	svc.mu.Unlock()

	// Trigger the shim shutdown drain. It must SetReadDeadline on each
	// vmConn (unblocking every v2t bridge) and wait for every handler
	// to return before reporting back.
	shutDone := make(chan error, 1)
	go func() { shutDone <- svc.shutdown(ctx) }()

	select {
	case err := <-shutDone:
		if err != nil {
			t.Fatalf("shutdown returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not return; some Stream handlers are still in flight (their v2t bridges blocked in binary.Read). The shutdown callback must SetReadDeadline on every tracked vmConn and wait for all handlers to exit.")
	}

	// All Stream handlers must have returned.
	for i, d := range dones {
		select {
		case <-d:
		case <-time.After(time.Second):
			t.Fatalf("stream %d: handler did not return after shutdown", i)
		}
	}

	// Tracking map must be empty (every handler's defer ran).
	svc.mu.Lock()
	if got := len(svc.streams); got != 0 {
		svc.mu.Unlock()
		t.Fatalf("expected streams map empty after shutdown, got %d remaining", got)
	}
	svc.mu.Unlock()
}

// TestStreamRejectedAfterShutdown asserts that the streaming service
// refuses new Stream RPCs once shutdown has marked it closing, so that
// late-arriving requests cannot register a vmConn that the shutdown
// drain has already finished waiting for.
func TestStreamRejectedAfterShutdown(t *testing.T) {
	ctx := t.Context()

	shimSide, vmSide := net.Pipe()
	t.Cleanup(func() { shimSide.Close(); vmSide.Close() })

	svc := &service{
		sb:      &fakeSandbox{conn: shimSide},
		streams: make(map[string]net.Conn),
	}

	if err := svc.shutdown(ctx); err != nil {
		t.Fatalf("shutdown returned %v", err)
	}

	srv := newFakeStreamServer(ctx)
	srv.recvCh <- streamInitAny(t, "post-shutdown")

	err := svc.Stream(ctx, srv)
	if !errors.Is(err, errdefs.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// fakeMultiSandbox returns a different net.Conn from StartStream for
// each registered stream ID, allowing a single ttrpc connection to host
// multiple streams with different VM-side behavior in the same test.
type fakeMultiSandbox struct {
	mu    sync.Mutex
	conns map[string]net.Conn
}

func (s *fakeMultiSandbox) Start(context.Context, ...sandbox.Opt) error {
	return errdefs.ErrNotImplemented
}
func (s *fakeMultiSandbox) Stop(context.Context) error     { return errdefs.ErrNotImplemented }
func (s *fakeMultiSandbox) Client() (*ttrpc.Client, error) { return nil, errdefs.ErrNotImplemented }
func (s *fakeMultiSandbox) StartStream(_ context.Context, id string) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, ok := s.conns[id]
	if !ok {
		return nil, fmt.Errorf("stream %q: %w", id, errdefs.ErrNotFound)
	}
	delete(s.conns, id)
	return conn, nil
}

// TestSlowStreamDoesNotBlockOtherStreams demonstrates the connection
// receive-loop deadlock fixed by the ttrpc upgrade in go.mod (v1.2.8 ->
// v1.2.9-...).
//
// In ttrpc v1.2.8 each server-side streamHandler has an unbuffered-ish
// recv channel (cap=5) and `streamHandler.data` blocks indefinitely
// when the channel is full. Because that function is called by the
// single per-connection receive goroutine in server.go, a stream whose
// handler is not draining its recv channel will block delivery for
// every other stream and unary RPC sharing the same client connection.
//
// In ttrpc v1.2.9 the buffer size is 64 for client-streaming RPCs and
// `data` falls back to a 1-second wait followed by ErrStreamFull, which
// keeps the receive loop moving even when one stream stalls.
//
// To reproduce: open a stream whose VM-side connection is wedged so
// the server's bridge consumes one message and then blocks on Write
// forever. Burst >65 messages on the wire to fill the recv buffer
// past the v1.2.9 cap, then open a second stream on the same client
// and try to complete its init handshake. With the old ttrpc the
// second stream's create+init request never reaches the handler;
// with the new ttrpc the slow stream's surplus messages return
// ErrStreamFull after ~1s and the second stream proceeds normally.
func TestSlowStreamDoesNotBlockOtherStreams(t *testing.T) {
	// VM-side conn for the "slow" stream: a synchronous net.Pipe whose
	// VM end is never read, so the bridge's first Write blocks forever
	// and subsequent client messages pile up in the stream's recv
	// channel until it overflows.
	slowShim, slowVM := net.Pipe()
	t.Cleanup(func() {
		slowShim.Close()
		slowVM.Close()
	})

	// VM-side conn for the "fast" stream: drained eagerly so the
	// handler can serve StreamInit and ack normally without back
	// pressure.
	fastShim, fastVM := net.Pipe()
	t.Cleanup(func() {
		fastShim.Close()
		fastVM.Close()
	})
	go io.Copy(io.Discard, fastVM)

	sb := &fakeMultiSandbox{
		conns: map[string]net.Conn{
			"slow": slowShim,
			"fast": fastShim,
		},
	}

	// Real ttrpc server with the streaming service registered.
	server, err := ttrpc.NewServer()
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	streamapi.RegisterTTRPCStreamingService(server, &service{sb: sb, streams: make(map[string]net.Conn)})

	// Stay under the AF_UNIX 104-byte sun_path limit on macOS:
	// t.TempDir() embeds the full test name, pushing the path over the
	// limit. os.MkdirTemp("", ...) uses os.TempDir() which produces a
	// short numeric-only subdirectory (~70 chars on macOS) and the correct
	// system temp dir on Windows.
	sockDir, err := os.MkdirTemp("", "nb-stream")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := server.Serve(serveCtx, listener); err != nil &&
			!errors.Is(err, ttrpc.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		// Closing the slow pipes first lets any blocked bridge writes
		// return so the streamHandler can finish and the receive loop
		// unblocks before we tear down the server.
		slowShim.Close()
		slowVM.Close()
		server.Close()
		serveCancel()
		<-serveDone
	})

	// ttrpc client over the same unix socket.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := ttrpc.NewClient(conn)
	t.Cleanup(func() { client.Close() })

	streamClient := streamapi.NewTTRPCStreamingClient(client)

	ctx := t.Context()

	// 1) Open the slow stream and complete its init handshake. The
	//    server's handler returns the slow shimSide conn from
	//    StartStream and starts its bridge goroutines; the bridge
	//    consumes the first data message we send below and then
	//    blocks forever on Write to slowShim.
	slow, err := streamClient.Stream(ctx)
	if err != nil {
		t.Fatalf("open slow stream: %v", err)
	}
	if err := slow.Send(streamInitAny(t, "slow")); err != nil {
		t.Fatalf("send slow init: %v", err)
	}
	if _, err := slow.Recv(); err != nil {
		t.Fatalf("slow ack: %v", err)
	}

	// 2) Burst messages on the slow stream until the server-side recv
	//    buffer overflows. With ttrpc v1.2.8 the buffer is 5, so any
	//    one of these will eventually wedge the connection's receive
	//    loop. With v1.2.9 the buffer is 64; we send a few more to
	//    push past it and trigger the 1-second ErrStreamFull fallback.
	//    66 = 1 consumed by the bridge + 64 buffered + 1 to overflow.
	payload := &anypb.Any{TypeUrl: "test/x", Value: bytes.Repeat([]byte{0xff}, 16)}
	for i := 0; i < 66; i++ {
		if err := slow.Send(payload); err != nil {
			// v1.2.9+ closes the stream with a status error after the
			// buffer-full event; further Sends will fail. By that
			// point the deadlock has already been provoked.
			break
		}
	}

	// 3) Open a second stream on the same client and complete its
	//    init handshake. With ttrpc v1.2.8 the create+init messages
	//    sit on the wire because the server's connection-wide receive
	//    goroutine is blocked delivering data to the slow stream's
	//    full recv buffer. With v1.2.9 the slow stream's surplus
	//    messages time out after ~1s with ErrStreamFull and the
	//    receive loop continues to process this stream.
	fast, err := streamClient.Stream(ctx)
	if err != nil {
		t.Fatalf("open fast stream: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		if err := fast.Send(streamInitAny(t, "fast")); err != nil {
			done <- fmt.Errorf("send fast init: %w", err)
			return
		}
		_, err := fast.Recv()
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fast stream init failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast stream init deadlocked: the slow stream's full recv buffer is blocking the connection's shared receive goroutine, so the fast stream's create+init request never reaches its handler. This is the deadlock fixed by upgrading ttrpc to v1.2.9 (per-stream recv buffer + ErrStreamFull fallback)")
	}
}
