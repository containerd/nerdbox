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

package podns

import (
	"context"

	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	api "github.com/containerd/nerdbox/api/services/podns/v1"
	"github.com/containerd/nerdbox/internal/vminit/podns"
	"github.com/containerd/nerdbox/plugins"
)

var _ api.TTRPCPodNamespacesService = &service{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   plugins.TTRPCPlugin,
		ID:     "podns",
		InitFn: initFunc,
	})
}

func initFunc(ic *plugin.InitContext) (interface{}, error) {
	return &service{}, nil
}

// service implements the PodNamespaces TTRPC service declared in
// podns.proto by delegating to a podns.Manager. See that package for why
// this exists as an on-demand RPC (called once per sandbox, the first
// time it's needed) rather than something created unconditionally at
// vminitd startup the way the shared network namespace is.
type service struct {
	mgr podns.Manager
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCPodNamespacesService(server, s)
	return nil
}

func (s *service) EnsureNamespaces(ctx context.Context, _ *api.EnsureNamespacesRequest) (*api.EnsureNamespacesResponse, error) {
	ipcPath, pidPath, err := s.mgr.EnsureNamespaces(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.EnsureNamespacesResponse{
		IpcNamespacePath: ipcPath,
		PidNamespacePath: pidPath,
	}, nil
}
