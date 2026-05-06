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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	streamapi "github.com/containerd/containerd/api/services/streaming/v1"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "streaming",
		Requires: []plugin.Type{
			cplugins.InternalPlugin,
			plugins.SandboxPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ss, err := ic.GetByID(cplugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			sb, err := ic.GetSingle(plugins.SandboxPlugin)
			if err != nil {
				return nil, err
			}
			s := &service{
				sb:      sb.(sandbox.Sandbox),
				streams: make(map[string]net.Conn),
			}
			ss.(shutdown.Service).RegisterCallback(s.shutdown)
			return s, nil
		},
	})
}

// maxFrameSize is the maximum allowed frame payload (10 MiB).
const maxFrameSize = 10 << 20

type service struct {
	sb sandbox.Sandbox

	mu      sync.Mutex
	streams map[string]net.Conn
	closing bool
	wg      sync.WaitGroup
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	streamapi.RegisterTTRPCStreamingService(server, s)
	return nil
}

func (s *service) Stream(ctx context.Context, srv streamapi.TTRPCStreaming_StreamServer) error {
	// Receive the StreamInit message with the stream ID
	a, err := srv.Recv()
	if err != nil {
		return err
	}
	var i streamapi.StreamInit
	if err := typeurl.UnmarshalTo(a, &i); err != nil {
		return err
	}

	log.G(ctx).WithField("stream", i.ID).Debug("creating stream bridge")

	// Create a stream connection to the VM, passing through the stream ID
	vmConn, err := s.sb.StartStream(ctx, i.ID)
	if err != nil {
		return fmt.Errorf("failed to start vm stream: %w", err)
	}

	// Track the stream so the shim shutdown callback can drain all
	// in-flight bridges via SetReadDeadline before sandbox.Stop tears
	// down the VM. If the shim is already shutting down, reject the
	// new stream rather than racing against teardown.
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		vmConn.Close()
		return fmt.Errorf("streaming plugin is shutting down: %w", errdefs.ErrUnavailable)
	}
	s.streams[i.ID] = vmConn
	s.wg.Add(1)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.streams, i.ID)
		s.mu.Unlock()
		vmConn.Close()
		s.wg.Done()
	}()

	log.G(ctx).WithField("stream", i.ID).Debug("stream bridge established")

	// Send ack back to containerd client
	e, _ := typeurl.MarshalAnyToProto(&ptypes.Empty{})
	if err := srv.Send(e); err != nil {
		return err
	}

	// Bridge messages in both directions as length-prefixed proto frames.

	// Client -> server: forward incoming messages to the VM.
	go func() {
		err := bridgeTTRPCToVM(srv, vmConn)
		// Signal end-of-stream to the VM with a zero-length frame.
		// We avoid CloseWrite() because the vsock proxy turns a
		// transport-level shutdown into a bidirectional SHUTDOWN,
		// which would kill the reverse direction and can cause the
		// peer to lose in-flight data.
		//
		// Log the EOF marker failure independently: when the bridge
		// returns io.EOF (client CloseSend) the dropped marker would
		// otherwise be lost both by the err==nil check and the
		// io.EOF filter on the bridge log below, leaving the VM
		// hanging while waiting for the marker.
		if eofErr := binary.Write(vmConn, binary.BigEndian, uint32(0)); eofErr != nil {
			log.G(ctx).WithError(eofErr).WithField("stream", i.ID).Debug("failed to write EOF marker to vm")
		}
		if err != nil && !errors.Is(err, io.EOF) {
			log.G(ctx).WithError(err).WithField("stream", i.ID).Debug("client->server bridge ended")
		}
	}()

	// Server -> client: forward VM messages back to the caller.
	v2t := make(chan error, 1)
	go func() {
		v2t <- bridgeVMToTTRPC(vmConn, srv)
	}()

	// The protocol contract is that the server initiates the close:
	// once it has finished its work it writes a zero-length frame,
	// which bridgeVMToTTRPC observes and returns nil for. Returning
	// before that signal would race the deferred vmConn.Close()
	// against the server's in-flight reads/writes and could silently
	// drop data still buffered in the kernel.
	//
	// We deliberately do not also wait on the client->server
	// direction: it can stay blocked in srv.Recv() if the client only
	// issues CloseSend after observing the server's EOF, which itself
	// requires this handler to return so ttrpc closes the server
	// stream. Waiting would deadlock. Instead the goroutine exits on
	// its own when ttrpc cancels the stream context on handler
	// return (or when vmConn.Close unblocks an in-flight write).
	select {
	case err := <-v2t:
		if err != nil && !errors.Is(err, io.EOF) {
			log.G(ctx).WithError(err).WithField("stream", i.ID).Debug("server->client bridge ended")
		}
	case <-ctx.Done():
		// On Windows, the AF_UNIX <-> vsock proxy turns vmConn.Close()
		// into a vsock SHUTDOWN that cascades into Task.Kill ->
		// Delete -> Shutdown when fired while the VM is mid-stream.
		// SetReadDeadline interrupts the bridge's binary.Read via the
		// Go runtime poller (no wire-level packet) so v2t drains
		// cleanly before the deferred Close() runs. See
		// docker/sandboxes#2529.
		if err := vmConn.SetReadDeadline(time.Now()); err != nil {
			log.G(ctx).WithError(err).WithField("stream", i.ID).Debug("failed to set read deadline on vm conn")
		}
		<-v2t
	}

	return nil
}

// shutdown is registered as a shim shutdown callback. It drains every
// in-flight Stream handler before sandbox.Stop tears down the VM so
// that all bridges have exited cleanly and the deferred vmConn.Close()
// calls have already fired by the time the shim/ttrpc transport goes
// away. Without this serialisation, N concurrent Stream handlers race
// the transport teardown on shim shutdown and leak partially-drained
// bridges that re-trigger the Windows SHUTDOWN cascade documented in
// docker/sandboxes#2529.
func (s *service) shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	streams := make([]net.Conn, 0, len(s.streams))
	for _, c := range s.streams {
		streams = append(streams, c)
	}
	s.mu.Unlock()

	for _, c := range streams {
		if err := c.SetReadDeadline(time.Now()); err != nil {
			log.G(ctx).WithError(err).Debug("failed to set read deadline on vm conn during shutdown")
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// bridgeTTRPCToVM reads typeurl.Any messages from the TTRPC stream and
// writes them as length-prefixed proto frames to the VM connection.
func bridgeTTRPCToVM(srv streamapi.TTRPCStreaming_StreamServer, conn io.Writer) error {
	for {
		a, err := srv.Recv()
		if err != nil {
			return err
		}

		data, err := proto.Marshal(typeurl.MarshalProto(a))
		if err != nil {
			return fmt.Errorf("failed to marshal for vm: %w", err)
		}
		if err := binary.Write(conn, binary.BigEndian, uint32(len(data))); err != nil {
			return fmt.Errorf("failed to write frame length to vm: %w", err)
		}
		if _, err := conn.Write(data); err != nil {
			return fmt.Errorf("failed to write frame data to vm: %w", err)
		}
	}
}

// bridgeVMToTTRPC reads length-prefixed proto frames from the VM
// connection and sends them as typeurl.Any messages on the TTRPC stream.
func bridgeVMToTTRPC(conn io.Reader, srv streamapi.TTRPCStreaming_StreamServer) error {
	for {
		var length uint32
		if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
			return err
		}
		// A zero-length frame is an application-level EOF marker.
		if length == 0 {
			return nil
		}
		if length > maxFrameSize {
			return fmt.Errorf("frame size %d exceeds maximum %d", length, maxFrameSize)
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(conn, data); err != nil {
			return fmt.Errorf("failed to read frame data from vm: %w", err)
		}
		var a anypb.Any
		if err := proto.Unmarshal(data, &a); err != nil {
			return fmt.Errorf("failed to unmarshal from vm: %w", err)
		}
		if err := srv.Send(&a); err != nil {
			return err
		}
	}
}
