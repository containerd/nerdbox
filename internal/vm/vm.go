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

// Package vm defines the interface for vm managers and instances
package vm

import (
	"context"
	"net"

	"github.com/containerd/ttrpc"
)

type Manager interface {
	NewInstance(ctx context.Context, state string) (Instance, error)
}

type startOpts struct {
}

type StartOpt func(*startOpts)

type mountOpts struct {
}

type MountOpt func(*mountOpts)

type Instance interface {
	AddFS(ctx context.Context, tag, mountPath string, opts ...MountOpt) error
	Start(ctx context.Context, opts ...StartOpt) error
	Client() *ttrpc.Client
	Shutdown(context.Context) error

	// StartStream makes a connection to the VM for streaming, returning a 32-bit
	// identifier for the stream that can be used to reference the stream inside
	// the vm.
	//
	// TODO: Consider making this interface optional, a per RPC implementation
	// is possible but likely less efficient.
	StartStream(ctx context.Context) (uint32, net.Conn, error)
}
