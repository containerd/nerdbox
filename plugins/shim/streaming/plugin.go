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
		// Half-close the write side so the VM sees EOF on its reads
		// while still allowing data to flow back from VM -> TTRPC.
		if cw, ok := vmConn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- err
	}()

	// VM -> TTRPC: read framed messages from VM, send to containerd
	go func() {
		done <- bridgeVMToTTRPC(vmConn, srv)
	}()

	// Wait for both bridge directions to finish or context cancellation.
	// We must not return early after just one direction finishes, because:
	// 1. Returning closes the TTRPC server stream which can race with
	//    other in-flight RPCs (e.g. Transfer) on the same connection.
	// 2. Closing vmConn eagerly can truncate in-flight data that the
	//    VM hasn't read yet.
	// Instead, wait for both to finish naturally. For unidirectional
	// streams, one direction will block until context cancellation
	// (shim shutdown), which is correct — the stream stays alive as
	// long as the connection does.
	for n := 0; n < 2; n++ {
		select {
		case err := <-done:
			if err != nil {
				log.G(ctx).WithError(err).WithField("stream", i.ID).Debug("stream bridge direction ended")
			}
		case <-ctx.Done():
			return nil
		}
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
