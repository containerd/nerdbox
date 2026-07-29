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

package namespaces

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	api "github.com/containerd/nerdbox/api/services/namespaces/v1"
	"github.com/containerd/nerdbox/internal/vminit/namespaces"
	"github.com/containerd/nerdbox/plugins"
)

var _ api.TTRPCNamespaceManagerService = &service{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   plugins.TTRPCPlugin,
		ID:     "namespaces",
		InitFn: initFunc,
	})
}

func initFunc(ic *plugin.InitContext) (interface{}, error) {
	return &service{}, nil
}

// service implements the NamespaceManager TTRPC service by delegating to a
// namespaces.Manager, translating the generated protobuf types to and from
// that package's own domain types.
type service struct {
	mgr namespaces.Manager
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCNamespaceManagerService(server, s)
	return nil
}

func (s *service) Create(ctx context.Context, r *api.CreateRequest) (*api.CreateResponse, error) {
	types, err := fromAPITypes(r.GetTypes())
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	paths, err := s.mgr.Create(ctx, r.GetID(), types)
	if err != nil {
		return nil, errgrpc.ToGRPC(toErrdefs(err))
	}

	// One entry per requested type, in request order.
	resp := &api.CreateResponse{Namespaces: make([]*api.Namespace, 0, len(types))}
	for _, typ := range types {
		path, ok := paths[typ]
		if !ok {
			return nil, errgrpc.ToGRPC(fmt.Errorf("no path for %s namespace: %w", typ, errdefs.ErrFailedPrecondition))
		}
		resp.Namespaces = append(resp.Namespaces, &api.Namespace{
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
func fromAPITypes(in []api.NamespaceType) ([]namespaces.Type, error) {
	out := make([]namespaces.Type, 0, len(in))
	for _, t := range in {
		switch t {
		case api.NamespaceType_NAMESPACE_TYPE_IPC:
			out = append(out, namespaces.TypeIPC)
		case api.NamespaceType_NAMESPACE_TYPE_PID:
			out = append(out, namespaces.TypePID)
		case api.NamespaceType_NAMESPACE_TYPE_NETWORK:
			out = append(out, namespaces.TypeNetwork)
		default:
			return nil, fmt.Errorf("unsupported namespace type %q: %w", t, errdefs.ErrInvalidArgument)
		}
	}
	return out, nil
}

func toAPIType(t namespaces.Type) api.NamespaceType {
	switch t {
	case namespaces.TypeIPC:
		return api.NamespaceType_NAMESPACE_TYPE_IPC
	case namespaces.TypePID:
		return api.NamespaceType_NAMESPACE_TYPE_PID
	case namespaces.TypeNetwork:
		return api.NamespaceType_NAMESPACE_TYPE_NETWORK
	default:
		return api.NamespaceType_NAMESPACE_TYPE_UNSPECIFIED
	}
}

// toErrdefs maps the Manager's validation failures onto an errdefs error so
// the caller sees InvalidArgument rather than Unknown.
func toErrdefs(err error) error {
	if errors.Is(err, namespaces.ErrInvalidArgument) {
		return fmt.Errorf("%s: %w", err.Error(), errdefs.ErrInvalidArgument)
	}
	return err
}
