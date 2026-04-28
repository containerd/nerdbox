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

	streamapi "github.com/containerd/containerd/api/services/streaming/v1"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	cplugins "github.com/containerd/containerd/v2/plugins"
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
			plugins.SandboxPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			sb, err := ic.GetSingle(plugins.SandboxPlugin)
			if err != nil {
				return nil, err
			}

			return &service{
				sb: sb.(sandbox.Sandbox),
			}, nil
		},
	})
}

// maxFrameSize is the maximum allowed frame payload (10 MiB).
const maxFrameSize = 10 << 20

type service struct {
	sb sandbox.Sandbox
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
	defer vmConn.Close()

	log.G(ctx).WithField("stream", i.ID).Debug("stream bridge established")

	// Send ack back to containerd client
	e, _ := typeurl.MarshalAnyToProto(&ptypes.Empty{})
	if err := srv.Send(e); err != nil {
		return err
	}

	// Start bidirectional bridge between TTRPC and VM.
	// Messages are forwarded as length-prefixed proto frames.
	done := make(chan error, 2)

	// TTRPC -> VM: receive typeurl.Any from containerd, frame and write to VM
	go func() {
		err := bridgeTTRPCToVM(srv, vmConn)
		// Send a zero-length frame as an application-level EOF marker
		// so the VM sees EOF on its reads. We avoid CloseWrite()
		// because the vsock proxy turns transport-level shutdown into
		// a bidirectional SHUTDOWN, which kills the reverse direction
		// (VM -> TTRPC) and can cause the peer to lose in-flight data.
		if eofErr := binary.Write(vmConn, binary.BigEndian, uint32(0)); eofErr != nil && err == nil {
			err = fmt.Errorf("failed to write EOF marker to vm: %w", eofErr)
		}
		done <- err
	}()

	// VM -> TTRPC: read framed messages from VM, send to containerd
	go func() {
		done <- bridgeVMToTTRPC(vmConn, srv)
	}()

	// Return as soon as one bridge direction finishes. That is the
	// signal that the stream's useful work is done:
	//   - bridgeVMToTTRPC EOF  → VM closed its side; all data delivered.
	//   - bridgeTTRPCToVM EOF  → daemon closed its send; the zero-length
	//                            frame above already signaled EOF to the VM.
	//
	// Returning here closes the TTRPC server stream, which the daemon's
	// ReceiveStream needs to unblock (stream.Recv returns io.EOF). If we
	// waited for both directions, bridgeTTRPCToVM would stay blocked on
	// srv.Recv whenever the daemon has stopped sending (e.g. for small
	// transfers where its flow-control window is not depleted), leaving
	// the server stream open indefinitely.
	//
	// The other goroutine cleans up on its own: defer vmConn.Close
	// below unblocks any pending Write, and ttrpc cancels the stream's
	// context after this handler returns, which unblocks any pending
	// srv.Recv.
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			log.G(ctx).WithError(err).WithField("stream", i.ID).Debug("stream bridge direction ended")
		}
	case <-ctx.Done():
	}

	return nil
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
