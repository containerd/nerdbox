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

	"github.com/containerd/containerd/v2/core/streaming"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/typeurl/v2"
	"github.com/mdlayher/vsock"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/containerd/nerdbox/plugins"
)

type serviceConfig struct {
	ContextID uint32
	Port      uint32
}

func (config *serviceConfig) SetVsock(cid, port uint32) {
	config.ContextID = cid
	config.Port = port
}

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.StreamingPlugin,
		ID:   "vsock",
		Requires: []plugin.Type{
			cplugins.InternalPlugin,
		},
		Config: &serviceConfig{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ss, err := ic.GetByID(cplugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			config := ic.Config.(*serviceConfig)
			l, err := vsock.ListenContextID(config.ContextID, config.Port, &vsock.Config{})
			if err != nil {
				return nil, fmt.Errorf("failed to listen on vsock port %d with context id %d: %w", config.Port, config.ContextID, err)
			}

			s := &service{
				l:       l,
				streams: make(map[string]*registration),
				pending: make(map[net.Conn]struct{}),
			}

			ss.(shutdown.Service).RegisterCallback(s.Shutdown)

			go s.Run()

			return s, nil
		},
	})
}

type service struct {
	mu sync.Mutex
	l  net.Listener

	streams map[string]*registration
	pending map[net.Conn]struct{}
	closed  bool
}

type registration struct {
	conn  net.Conn
	ready chan struct{}
	err   error
}

const handshakeTimeout = 15 * time.Second

func (s *service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	var errs []error

	clear(s.streams)
	for conn := range s.pending {
		if err := conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close connection: %w", err))
		}
	}

	if s.l != nil {
		if err := s.l.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close listener: %w", err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (s *service) Run() {
	var backoff time.Duration
	for {
		conn, err := s.l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else {
				backoff = min(backoff*2, time.Second)
			}
			log.L.WithError(err).Warn("failed to accept stream connection; retrying")
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.pending[conn] = struct{}{}
		s.mu.Unlock()
		go s.acceptStream(conn)
	}
}

func (s *service) acceptStream(conn net.Conn) {
	registered := false
	defer func() {
		if registered {
			return
		}
		s.mu.Lock()
		delete(s.pending, conn)
		s.mu.Unlock()
		conn.Close()
	}()
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	var idLen uint32
	if err := binary.Read(conn, binary.BigEndian, &idLen); err != nil {
		log.L.WithError(err).Debug("failed to read stream ID length")
		return
	}
	// Allocate only for bytes received, not the peer's advertised length.
	idBytes, err := io.ReadAll(io.LimitReader(conn, int64(idLen)))
	if err == nil && int64(len(idBytes)) != int64(idLen) {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		log.L.WithError(err).Debug("failed to read stream ID")
		return
	}
	streamID := string(idBytes)
	r := &registration{conn: conn, ready: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if _, ok := s.streams[streamID]; ok {
		s.mu.Unlock()
		writeString(conn, fmt.Sprintf("stream %q already exists", streamID))
		return
	}
	s.streams[streamID] = r
	s.mu.Unlock()

	r.err = writeString(conn, streamID)
	if r.err == nil {
		r.err = conn.SetDeadline(time.Time{})
	}
	if r.err != nil {
		s.removeStream(streamID, r)
	} else {
		registered = true
	}
	close(r.ready)
}

func (s *service) removeStream(streamID string, r *registration) {
	s.mu.Lock()
	if s.streams[streamID] == r {
		delete(s.streams, streamID)
		delete(s.pending, r.conn)
		r.conn.Close()
	}
	s.mu.Unlock()
}

// writeString writes a length-prefixed string to the connection.
func writeString(conn net.Conn, s string) error {
	b := []byte(s)
	if err := binary.Write(conn, binary.BigEndian, uint32(len(b))); err != nil {
		return err
	}
	_, err := conn.Write(b)
	return err
}

// Get returns the raw connection for the given stream ID, removing it from
// the map. This implements stream.Manager for the task service IO forwarding.
func (s *service) Get(id string) (io.ReadWriteCloser, error) {
	s.mu.Lock()
	r, ok := s.streams[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("stream %q not found: %w", id, errdefs.ErrNotFound)
	}
	delete(s.streams, id)
	s.mu.Unlock()
	// Registration precedes the ACK so a fast claimant cannot miss the ID.
	// Ownership transfers only once the handshake deadline has been cleared.
	<-r.ready
	s.mu.Lock()
	delete(s.pending, r.conn)
	s.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return r.conn, nil
}

// StreamGetter returns a streaming.StreamGetter that looks up streams by
// their string stream ID.
func (s *service) StreamGetter() streaming.StreamGetter {
	return &streamGetter{s: s}
}

type streamGetter struct {
	s *service
}

func (sg *streamGetter) Get(ctx context.Context, name string) (streaming.Stream, error) {
	conn, err := sg.s.Get(name)
	if err != nil {
		return nil, err
	}
	return &vsockStream{conn: conn.(net.Conn)}, nil
}

// maxFrameSize is the maximum allowed frame payload (10 MiB). Frames
// larger than this are rejected to prevent OOM from buggy/malicious peers.
const maxFrameSize = 10 << 20

// vsockStream wraps a net.Conn with length-prefixed proto framing to
// implement the streaming.Stream interface. Each message is framed as
// a 4-byte big-endian length prefix followed by serialized proto bytes.
type vsockStream struct {
	conn        net.Conn
	once        sync.Once // ensures Close sends EOF exactly once
	mu          sync.Mutex
	readClosed  bool
	writeClosed bool
}

func (s *vsockStream) Send(a typeurl.Any) error {
	data, err := proto.Marshal(typeurl.MarshalProto(a))
	if err != nil {
		return fmt.Errorf("failed to marshal stream message: %w", err)
	}
	if err := binary.Write(s.conn, binary.BigEndian, uint32(len(data))); err != nil {
		return fmt.Errorf("failed to write frame length: %w", err)
	}
	if _, err := s.conn.Write(data); err != nil {
		return fmt.Errorf("failed to write frame data: %w", err)
	}
	return nil
}

func (s *vsockStream) Recv() (_ typeurl.Any, retErr error) {
	defer func() {
		if retErr == nil {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.readClosed = true
		if s.writeClosed || !errors.Is(retErr, io.EOF) {
			s.conn.Close()
		}
	}()
	var length uint32
	if err := binary.Read(s.conn, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	// A zero-length frame is an application-level EOF marker.
	if length == 0 {
		return nil, io.EOF
	}
	if length > maxFrameSize {
		return nil, fmt.Errorf("frame size %d exceeds maximum %d", length, maxFrameSize)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(s.conn, data); err != nil {
		return nil, fmt.Errorf("failed to read frame data: %w", err)
	}
	var a anypb.Any
	if err := proto.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stream message: %w", err)
	}
	return &a, nil
}

func (s *vsockStream) Close() error {
	var err error
	s.once.Do(func() {
		// The receive direction may still carry data after our send EOF.
		err = binary.Write(s.conn, binary.BigEndian, uint32(0))
		s.mu.Lock()
		defer s.mu.Unlock()
		s.writeClosed = true
		if s.readClosed || err != nil {
			s.conn.Close()
		}
	})
	return err
}
