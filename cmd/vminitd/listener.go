//go:build linux

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

package main

import (
	"fmt"
	"net"
	"sync"

	"github.com/mdlayher/vsock"
)

// dialBackListener is a net.Listener that dials a vsock connection to the
// host on the first Accept call, then blocks on subsequent calls until
// Close is called. This allows a ttrpc.Server to serve a single connection
// initiated by dialing the host rather than listening for connections.
type dialBackListener struct {
	cid  uint32
	port uint32

	once    sync.Once
	done    chan struct{}
	dialErr error
}

func newDialBackListener(cid, port uint32) *dialBackListener {
	return &dialBackListener{
		cid:  cid,
		port: port,
		done: make(chan struct{}),
	}
}

func (l *dialBackListener) Accept() (net.Conn, error) {
	var (
		conn net.Conn
		dial bool
	)
	l.once.Do(func() {
		dial = true
		conn, l.dialErr = vsock.Dial(l.cid, l.port, nil)
		if l.dialErr != nil {
			close(l.done)
		}
	})
	if dial {
		if l.dialErr != nil {
			return nil, fmt.Errorf("failed to dial host vsock %d:%d: %w", l.cid, l.port, l.dialErr)
		}
		return conn, nil
	}
	// Block until the listener is closed.
	<-l.done
	return nil, net.ErrClosed
}

func (l *dialBackListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *dialBackListener) Addr() net.Addr {
	return &vsock.Addr{
		ContextID: l.cid,
		Port:      l.port,
	}
}
