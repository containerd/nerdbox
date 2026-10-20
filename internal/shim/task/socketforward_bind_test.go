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

package task

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/containerd/ttrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	socketforward "github.com/containerd/nerdbox/api/services/socketforward/v1"
	"github.com/containerd/nerdbox/internal/shim/sandbox"
)

type socketForwardTestSandbox struct {
	sandbox.Sandbox
	client *ttrpc.Client
}

func (s socketForwardTestSandbox) Client() (*ttrpc.Client, error) { return s.client, nil }

func TestBindSocketsPassesRequestedMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode uint32
	}{
		{name: "legacy"},
		{name: "private", mode: 0600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, err := ttrpc.NewServer()
			require.NoError(t, err)
			bindRequests := make(chan *socketforward.BindRequest, 1)
			server.Register("nerdbox.services.socketforward.v1.SocketForward", map[string]ttrpc.Method{
				"Bind": func(_ context.Context, unmarshal func(interface{}) error) (interface{}, error) {
					var req socketforward.BindRequest
					if err := unmarshal(&req); err != nil {
						return nil, err
					}
					bindRequests <- &req
					return &emptypb.Empty{}, nil
				},
			})
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			go func() { _ = server.Serve(t.Context(), listener) }()
			conn, err := net.Dial("tcp", listener.Addr().String())
			require.NoError(t, err)
			client := ttrpc.NewClient(conn)
			t.Cleanup(func() {
				require.NoError(t, client.Close())
				require.NoError(t, server.Close())
			})

			err = bindSockets(t.Context(), socketForwardTestSandbox{client: client}, []socketForwardEntry{{
				id: "test", vmPath: "/run/socketfwd/test.sock", mode: tc.mode,
				uid: 1000, gid: 1000,
			}})
			require.NoError(t, err)
			req := <-bindRequests
			require.Len(t, req.Sockets, 1)
			require.Equal(t, tc.mode, req.Sockets[0].Mode)
			require.Equal(t, uint32(1000), req.Sockets[0].Uid)
			require.Equal(t, uint32(1000), req.Sockets[0].Gid)
		})
	}
}

func TestBindSocketsRejectsGuestFailure(t *testing.T) {
	server, err := ttrpc.NewServer()
	require.NoError(t, err)
	server.Register("nerdbox.services.socketforward.v1.SocketForward", map[string]ttrpc.Method{
		"Bind": func(context.Context, func(interface{}) error) (interface{}, error) {
			return nil, errors.New("guest rejected mode")
		},
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(t.Context(), listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	client := ttrpc.NewClient(conn)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
		require.NoError(t, server.Close())
	})
	require.ErrorContains(t, bindSockets(t.Context(), socketForwardTestSandbox{client: client}, []socketForwardEntry{{mode: 0600}}), "guest rejected mode")
}
