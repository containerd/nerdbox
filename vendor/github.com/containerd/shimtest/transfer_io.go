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
	"errors"
	"io"

	streamingapi "github.com/containerd/containerd/api/services/streaming/v1"
	"github.com/containerd/containerd/v2/core/streaming"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/types/known/anypb"
)

// streamMarshaler is implemented by types that create streams during
// marshaling.
type streamMarshaler interface {
	MarshalAny(context.Context, streaming.StreamCreator) (typeurl.Any, error)
}

// marshalTransferAny marshals a transfer source/destination to an
// anypb.Any. If v implements streamMarshaler (ReadStream, WriteStream),
// the streaming variant is used so the stream is registered with sc.
func marshalTransferAny(ctx context.Context, v any, sc streaming.StreamCreator) (*anypb.Any, error) {
	var a typeurl.Any
	var err error
	if sm, ok := v.(streamMarshaler); ok {
		a, err = sm.MarshalAny(ctx, sc)
	} else {
		a, err = typeurl.MarshalAny(v)
	}
	if err != nil {
		return nil, err
	}
	return &anypb.Any{
		TypeUrl: a.GetTypeUrl(),
		Value:   a.GetValue(),
	}, nil
}

// ttrpcStreamCreator implements streaming.StreamCreator over TTRPC.
// It opens a bidirectional stream to the shim's streaming service,
// sends a StreamInit with the stream id, waits for the ack, and
// returns a Stream that wraps the TTRPC bidi stream.
type ttrpcStreamCreator struct {
	client streamingapi.TTRPCStreamingClient
}

func (sc *ttrpcStreamCreator) Create(ctx context.Context, id string) (streaming.Stream, error) {
	stream, err := sc.client.Stream(ctx)
	if err != nil {
		return nil, toNative(err)
	}

	a, err := typeurl.MarshalAny(&streamingapi.StreamInit{ID: id})
	if err != nil {
		return nil, err
	}
	if err := stream.Send(typeurl.MarshalProto(a)); err != nil {
		return nil, toNative(err)
	}

	if _, err := stream.Recv(); err != nil {
		return nil, toNative(err)
	}

	return &ttrpcStream{s: stream}, nil
}

// ttrpcStream wraps a ttrpc bidi stream, satisfying streaming.Stream.
type ttrpcStream struct {
	s streamingapi.TTRPCStreaming_StreamClient
}

func (cs *ttrpcStream) Send(a typeurl.Any) error {
	return toNative(cs.s.Send(typeurl.MarshalProto(a)))
}

func (cs *ttrpcStream) Recv() (typeurl.Any, error) {
	a, err := cs.s.Recv()
	if err != nil {
		return nil, toNative(err)
	}
	return a, nil
}

func (cs *ttrpcStream) Close() error {
	return cs.s.CloseSend()
}

// toNative converts ttrpc status errors to native Go errors,
// preserving context errors directly so errors.Is checks work.
func toNative(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errgrpc.ToNative(err)
}
