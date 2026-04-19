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

// Package transfer defines transfer types for container filesystem operations.
package transfer

import (
	"context"
	"io"

	transferpb "github.com/containerd/containerd/api/types/transfer"
	"github.com/containerd/containerd/v2/core/streaming"
	tplugins "github.com/containerd/containerd/v2/core/transfer/plugins"
	tstreaming "github.com/containerd/containerd/v2/core/transfer/streaming"
	"github.com/containerd/typeurl/v2"
)

func init() {
	tplugins.Register(&transferpb.ContainerPath{}, ContainerPath{})
	tplugins.Register(&transferpb.ReadStream{}, ReadStream{})
	tplugins.Register(&transferpb.WriteStream{}, WriteStream{})
}

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
func (cf *ContainerPath) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
	return typeurl.MarshalAny(&transferpb.ContainerPath{
		ContainerID:       cf.ContainerID,
		Path:              cf.Path,
		NoWalk:            cf.NoWalk,
		PreserveOwnership: cf.PreserveOwnership,
	})
}

// UnmarshalAny unmarshals a ContainerPath from a typeurl.Any.
func (cf *ContainerPath) UnmarshalAny(ctx context.Context, sg streaming.StreamGetter, a typeurl.Any) error {
	var p transferpb.ContainerPath
	if err := typeurl.UnmarshalTo(a, &p); err != nil {
		return err
	}
	cf.ContainerID = p.ContainerID
	cf.Path = p.Path
	cf.NoWalk = p.NoWalk
	cf.PreserveOwnership = p.PreserveOwnership
	return nil
}

// ReadStream carries data from the client to the server (import
// direction). The client sends data through the stream and the server
// reads it.
type ReadStream struct {
	MediaType string
	stream    streaming.Stream
	reader    io.Reader // client-side: set by constructor, used by MarshalAny
}

// NewReadStream creates a ReadStream that will send data from r to the
// server during marshaling.
func NewReadStream(r io.Reader, mediaType string) *ReadStream {
	return &ReadStream{MediaType: mediaType, reader: r}
}

// MarshalAny marshals the ReadStream, creating a streaming connection
// and starting a goroutine that sends data from the reader.
func (s *ReadStream) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
	sid := tstreaming.GenerateID("data")
	stream, err := sm.Create(ctx, sid)
	if err != nil {
		return nil, err
	}

	go tstreaming.SendStream(ctx, s.reader, stream)

	return typeurl.MarshalAny(&transferpb.ReadStream{
		Stream:    sid,
		MediaType: s.MediaType,
	})
}

// UnmarshalAny unmarshals a ReadStream from a typeurl.Any, recovering
// the stream from the StreamGetter.
func (s *ReadStream) UnmarshalAny(ctx context.Context, sg streaming.StreamGetter, a typeurl.Any) error {
	var p transferpb.ReadStream
	if err := typeurl.UnmarshalTo(a, &p); err != nil {
		return err
	}
	stream, err := sg.Get(ctx, p.Stream)
	if err != nil {
		return err
	}
	s.stream = stream
	s.MediaType = p.MediaType
	return nil
}

// Reader returns an io.Reader that consumes data sent by the client.
func (s *ReadStream) Reader(ctx context.Context) io.Reader {
	return tstreaming.ReceiveStream(ctx, s.stream)
}

// WriteStream carries data from the server to the client (export
// direction). The server writes data into the stream and the client
// receives it.
type WriteStream struct {
	MediaType string
	stream    streaming.Stream
	writer    io.WriteCloser // client-side: set by constructor, used by MarshalAny
}

// NewWriteStream creates a WriteStream that will receive data from the
// server into w during marshaling.
func NewWriteStream(w io.WriteCloser, mediaType string) *WriteStream {
	return &WriteStream{MediaType: mediaType, writer: w}
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
		io.Copy(s.writer, tstreaming.ReceiveStream(ctx, stream))
		s.writer.Close()
	}()

	return typeurl.MarshalAny(&transferpb.WriteStream{
		Stream:    sid,
		MediaType: s.MediaType,
	})
}

// UnmarshalAny unmarshals a WriteStream from a typeurl.Any, recovering
// the stream from the StreamGetter.
func (s *WriteStream) UnmarshalAny(ctx context.Context, sg streaming.StreamGetter, a typeurl.Any) error {
	var p transferpb.WriteStream
	if err := typeurl.UnmarshalTo(a, &p); err != nil {
		return err
	}
	stream, err := sg.Get(ctx, p.Stream)
	if err != nil {
		return err
	}
	s.stream = stream
	s.MediaType = p.MediaType
	return nil
}

// Writer returns an io.WriteCloser that sends data to the client.
func (s *WriteStream) Writer(ctx context.Context) io.WriteCloser {
	return tstreaming.WriteByteStream(ctx, s.stream)
}
