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

package transfer

import (
	"context"

	transferapi "github.com/containerd/containerd/api/services/transfer/v1"
	"github.com/containerd/containerd/v2/core/streaming"
	"github.com/containerd/containerd/v2/core/transfer"
	tplugins "github.com/containerd/containerd/v2/core/transfer/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	itransfer "github.com/containerd/nerdbox/internal/transfer"
	"github.com/containerd/nerdbox/plugins"
)

// streamGetterProvider is implemented by the vsock streaming plugin.
type streamGetterProvider interface {
	StreamGetter() streaming.StreamGetter
}

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "transfer",
		Requires: []plugin.Type{
			plugins.StreamingPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			sp, err := ic.GetByID(plugins.StreamingPlugin, "vsock")
			if err != nil {
				return nil, err
			}
			sgp := sp.(streamGetterProvider)

			bundleDir := ic.Properties[plugins.PropertyBundleDir]

			return &service{
				streamGetter: sgp.StreamGetter(),
				transferrers: []transfer.Transferrer{
					itransfer.NewContainerFSTransferrer(bundleDir),
					itransfer.NewEchoTransferrer(),
				},
			}, nil
		},
	})
}

type service struct {
	streamGetter streaming.StreamGetter
	transferrers []transfer.Transferrer
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	transferapi.RegisterTTRPCTransferService(server, s)
	return nil
}

func (s *service) Transfer(ctx context.Context, req *transferapi.TransferRequest) (*emptypb.Empty, error) {
	src, err := s.convertAny(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	dst, err := s.convertAny(ctx, req.Destination)
	if err != nil {
		return nil, err
	}

	for _, t := range s.transferrers {
		if err = t.Transfer(ctx, src, dst); err == nil {
			return &emptypb.Empty{}, nil
		} else if !errdefs.IsNotImplemented(err) {
			return nil, err
		}
		log.G(ctx).WithError(err).Debugf("transfer not implemented for %T to %T", src, dst)
	}
	return nil, status.Errorf(codes.Unimplemented, "method Transfer not implemented for %s to %s", req.Source.GetTypeUrl(), req.Destination.GetTypeUrl())
}

type streamUnmarshaler interface {
	UnmarshalAny(context.Context, streaming.StreamGetter, typeurl.Any) error
}

func (s *service) convertAny(ctx context.Context, a typeurl.Any) (any, error) {
	obj, err := tplugins.ResolveType(a)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return typeurl.UnmarshalAny(a)
		}
		return nil, err
	}
	switch v := obj.(type) {
	case streamUnmarshaler:
		err = v.UnmarshalAny(ctx, s.streamGetter, a)
		return obj, err
	default:
		err = typeurl.UnmarshalTo(a, obj)
		return obj, err
	}
}
