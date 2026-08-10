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

package sharedresources

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	api "github.com/containerd/nerdbox/api/services/sharedresources/v1"
	"github.com/containerd/nerdbox/internal/vminit/sharedresources"
	"github.com/containerd/nerdbox/plugins"
)

var _ api.TTRPCSharedResourcesService = &service{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   plugins.TTRPCPlugin,
		ID:     "sharedresources",
		InitFn: initFunc,
	})
}

func initFunc(ic *plugin.InitContext) (interface{}, error) {
	return &service{}, nil
}

// service implements the SharedResources TTRPC service by delegating to a
// sharedresources.Manager, translating the generated protobuf types to and
// from that package's own domain types.
type service struct {
	mgr sharedresources.Manager
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCSharedResourcesService(server, s)
	return nil
}

func (s *service) Create(ctx context.Context, r *api.CreateRequest) (*api.CreateResponse, error) {
	types, err := fromAPITypes(r.GetTypes())
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	paths, err := s.mgr.Create(ctx, r.GetID(), types, r.GetDevShmSizeBytes())
	if err != nil {
		return nil, errgrpc.ToGRPC(toErrdefs(err))
	}

	// One entry per requested type, in request order.
	resp := &api.CreateResponse{Resources: make([]*api.SharedResource, 0, len(types))}
	for _, typ := range types {
		path, ok := paths[typ]
		if !ok {
			return nil, errgrpc.ToGRPC(fmt.Errorf("no path for %s resource: %w", typ, errdefs.ErrFailedPrecondition))
		}
		resp.Resources = append(resp.Resources, &api.SharedResource{
			Type: toAPIType(typ),
			Path: path,
		})
	}
	return resp, nil
}

func (s *service) Delete(ctx context.Context, r *api.DeleteRequest) (*api.DeleteResponse, error) {
	types, err := fromAPITypes(r.GetTypes())
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if err := s.mgr.Delete(ctx, r.GetID(), types); err != nil {
		return nil, errgrpc.ToGRPC(toErrdefs(err))
	}
	return &api.DeleteResponse{}, nil
}

// fromAPITypes converts requested wire types to domain types, rejecting
// unspecified or unrecognized values.
func fromAPITypes(in []api.Type) ([]sharedresources.Type, error) {
	out := make([]sharedresources.Type, 0, len(in))
	for _, t := range in {
		switch t {
		case api.Type_TYPE_NAMESPACE_IPC:
			out = append(out, sharedresources.TypeNamespaceIPC)
		case api.Type_TYPE_NAMESPACE_PID:
			out = append(out, sharedresources.TypeNamespacePID)
		case api.Type_TYPE_NAMESPACE_NETWORK:
			out = append(out, sharedresources.TypeNamespaceNetwork)
		case api.Type_TYPE_DEVSHM:
			out = append(out, sharedresources.TypeDevShm)
		default:
			return nil, fmt.Errorf("unsupported resource type %q: %w", t, errdefs.ErrInvalidArgument)
		}
	}
	return out, nil
}

func toAPIType(t sharedresources.Type) api.Type {
	switch t {
	case sharedresources.TypeNamespaceIPC:
		return api.Type_TYPE_NAMESPACE_IPC
	case sharedresources.TypeNamespacePID:
		return api.Type_TYPE_NAMESPACE_PID
	case sharedresources.TypeNamespaceNetwork:
		return api.Type_TYPE_NAMESPACE_NETWORK
	case sharedresources.TypeDevShm:
		return api.Type_TYPE_DEVSHM
	default:
		return api.Type_TYPE_UNSPECIFIED
	}
}

// toErrdefs maps the Manager's validation failures onto an errdefs error so
// the caller sees InvalidArgument rather than Unknown.
func toErrdefs(err error) error {
	if errors.Is(err, sharedresources.ErrInvalidArgument) {
		return fmt.Errorf("%s: %w", err.Error(), errdefs.ErrInvalidArgument)
	}
	return err
}
