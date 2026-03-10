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
	"fmt"

	transferapi "github.com/containerd/containerd/api/services/transfer/v1"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "transfer",
		Requires: []plugin.Type{
			plugins.SandboxPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			sb, err := ic.GetSingle(plugins.SandboxPlugin)
			if err != nil {
				return nil, err
			}
			return &service{
				sandbox: sb.(sandbox.Sandbox),
			}, nil
		},
	})
}

type service struct {
	sandbox sandbox.Sandbox
}

func (s *service) RegisterTTRPC(ts *ttrpc.Server) error {
	transferapi.RegisterTTRPCTransferService(ts, s)
	return nil
}

func (s *service) Transfer(ctx context.Context, req *transferapi.TransferRequest) (*emptypb.Empty, error) {
	log.G(ctx).Debug("transfer: forwarding to vminitd")

	client, err := s.sandbox.Client()
	if err != nil {
		return nil, fmt.Errorf("failed to get vminitd client: %w", err)
	}

	vmTransfer := transferapi.NewTTRPCTransferClient(client)
	return vmTransfer.Transfer(ctx, req)
}
