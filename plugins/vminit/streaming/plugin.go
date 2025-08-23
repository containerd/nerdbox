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
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/mdlayher/vsock"

	"github.com/dmcgowan/nerdbox/plugins"
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
			l, err := vsock.ListenContextID(uint32(config.ContextID), uint32(config.Port), &vsock.Config{})
			if err != nil {
				return nil, fmt.Errorf("failed to listen on vsock port %d with context id %d: %w", config.Port, config.ContextID, err)
			}

			s := &service{
				l:       l,
				streams: make(map[uint32]net.Conn),
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

	streams map[uint32]net.Conn
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
		var b [4]byte
		if _, err := conn.Read(b[:]); err != nil {
			conn.Close()
			continue // Error reading, close connection
		}

		s.mu.Lock()
		sid := binary.BigEndian.Uint32(b[:])
		if _, ok := s.streams[sid]; ok {
			s.mu.Unlock()
			conn.Close()
			continue // Error reading, close connection
		}
		s.streams[sid] = streamConn{
			Conn: conn,
			sid:  sid,
			s:    s,
		}
		s.mu.Unlock()
		conn.Write(b[:])
	}
}

func (s *service) Get(id uint32) (io.ReadWriteCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.streams[id]
	if !ok {
		return nil, fmt.Errorf("stream %d not found: %w", id, errdefs.ErrNotFound)
	}
	return c, nil
}

type streamConn struct {
	net.Conn
	sid uint32
	s   *service
}

func (sc streamConn) Close() error {
	sc.s.mu.Lock()
	defer sc.s.mu.Unlock()
	delete(sc.s.streams, sc.sid)

	if err := sc.Conn.Close(); err != nil {
		return fmt.Errorf("failed to close connection: %w", err)
	}

	return nil
}
