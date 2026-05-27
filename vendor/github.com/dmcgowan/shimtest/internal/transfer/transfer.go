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

// Package transfer provides client-side transfer types that marshal
// into the containerd transfer API protobuf messages.
package transfer

import (
	"context"
	"io"

	transferpb "github.com/containerd/containerd/api/types/transfer"
	"github.com/containerd/containerd/v2/core/streaming"
	tstreaming "github.com/containerd/containerd/v2/core/transfer/streaming"
	"github.com/containerd/typeurl/v2"
)

// ContainerPath represents a path within a running container's
// filesystem. It acts as either a source or destination in a transfer
// operation, identifying the container and path for archive operations.
type ContainerPath struct {
	ContainerID       string
	Path              string
	NoWalk            bool
	PreserveOwnership bool
}

// MarshalAny marshals the ContainerPath to a typeurl.Any.
func (cp *ContainerPath) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
	return typeurl.MarshalAny(&transferpb.ContainerPath{
		ContainerID:       cp.ContainerID,
		Path:              cp.Path,
		NoWalk:            cp.NoWalk,
		PreserveOwnership: cp.PreserveOwnership,
	})
}

// ReadStream carries data from the client to the server (import
// direction). The client sends data through the stream and the server
// reads it.
//
// After calling Transfer, callers must call Wait to block until the
// background send goroutine has completed and to obtain any error it
// encountered. Skipping Wait silently discards send errors and races
// the goroutine against caller teardown.
type ReadStream struct {
	MediaType string
	reader    io.Reader
	done      chan struct{}
	err       error
}

// NewReadStream creates a ReadStream that will send data from r to the
// server during marshaling.
func NewReadStream(r io.Reader, mediaType string) *ReadStream {
	return &ReadStream{MediaType: mediaType, reader: r, done: make(chan struct{})}
}

// MarshalAny marshals the ReadStream, creating a streaming connection
// and starting a goroutine that sends data from the reader.
func (s *ReadStream) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
	sid := tstreaming.GenerateID("data")
	stream, err := sm.Create(ctx, sid)
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(s.done)
		tstreaming.SendStream(ctx, s.reader, stream)
	}()

	return typeurl.MarshalAny(&transferpb.ReadStream{
		Stream:    sid,
		MediaType: s.MediaType,
	})
}

// Wait blocks until the background send goroutine has finished and
// returns any error it encountered. It must be called after the
// Transfer RPC returns. ctx is used only for cancellation; if it
// fires before the goroutine exits, Wait returns ctx.Err().
//
// Note: SendStream does not currently return an error (errors are
// logged internally), so Wait primarily serves as a synchronization
// barrier to ensure the send goroutine has fully drained the reader
// before the caller proceeds.
func (s *ReadStream) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WriteStream carries data from the server to the client (export
// direction). The server writes data into the stream and the client
// receives it.
//
// After calling Transfer, callers must call Wait to block until the
// background receive goroutine has drained the stream into the
// underlying writer and to obtain any error it encountered. Skipping
// Wait races the goroutine against the caller reading the writer's
// buffer, and silently discards receive errors (producing an empty or
// truncated result indistinguishable from a successful empty
// response).
type WriteStream struct {
	MediaType string
	writer    io.Writer
	done      chan struct{}
	err       error
}

// NewWriteStream creates a WriteStream that will receive data from the
// server into w during marshaling.
func NewWriteStream(w io.Writer, mediaType string) *WriteStream {
	return &WriteStream{MediaType: mediaType, writer: w, done: make(chan struct{})}
}

// MarshalAny marshals the WriteStream, creating a streaming connection
// and starting a goroutine that receives data into the writer.
func (s *WriteStream) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
	sid := tstreaming.GenerateID("data")
	stream, err := sm.Create(ctx, sid)
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(s.done)
		_, s.err = io.Copy(s.writer, tstreaming.ReceiveStream(ctx, stream))
	}()

	return typeurl.MarshalAny(&transferpb.WriteStream{
		Stream:    sid,
		MediaType: s.MediaType,
	})
}

// Wait blocks until the background receive goroutine has finished
// draining the stream into the writer and returns any error it
// encountered. It must be called after the Transfer RPC returns.
// ctx is used only for cancellation; if it fires before the goroutine
// exits, Wait returns ctx.Err().
//
// The Transfer RPC returning successfully only means the server has
// finished writing — the client-side io.Copy from the bidi stream into
// the writer runs in a separate goroutine that may still be in flight.
// Calling Wait ensures the writer's buffer is fully populated before
// the caller reads it, and surfaces any transport error rather than
// silently delivering an empty or truncated result.
func (s *WriteStream) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
