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

	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/mdlayher/vsock"

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
				streams: make(map[string]net.Conn),
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

	streams map[string]net.Conn
}

func (s *service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error

	// Close all connections
	for _, conn := range s.streams {
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
	for {
		conn, err := s.l.Accept()
		if err != nil {
			return // Listener closed
		}

		// Read length-prefixed stream ID
		var idLen uint32
		if err := binary.Read(conn, binary.BigEndian, &idLen); err != nil {
			log.L.WithError(err).Debug("failed to read stream ID length")
			conn.Close()
			continue
		}
		idBytes := make([]byte, idLen)
		if _, err := io.ReadFull(conn, idBytes); err != nil {
			log.L.WithError(err).Debug("failed to read stream ID")
			conn.Close()
			continue
		}
		streamID := string(idBytes)

		s.mu.Lock()
		if _, ok := s.streams[streamID]; ok {
			s.mu.Unlock()
			log.L.WithField("stream", streamID).Debug("duplicate stream ID, rejecting")
			// Send back an error message so the client gets a meaningful rejection
			errMsg := fmt.Sprintf("stream %q already exists", streamID)
			writeString(conn, errMsg)
			conn.Close()
			continue
		}
		s.streams[streamID] = conn
		s.mu.Unlock()

		// Ack: echo back the stream ID
		if err := writeString(conn, streamID); err != nil {
			s.removeStream(streamID)
			conn.Close()
			continue
		}
	}
}

func (s *service) removeStream(streamID string) {
	s.mu.Lock()
	delete(s.streams, streamID)
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
	defer s.mu.Unlock()
	conn, ok := s.streams[id]
	if !ok {
		return nil, fmt.Errorf("stream %q not found: %w", id, errdefs.ErrNotFound)
	}
	delete(s.streams, id)
	return conn, nil
}
