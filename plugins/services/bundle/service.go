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

package bundle

import (
	"context"
	"os"
	"path/filepath"

	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	api "github.com/containerd/nerdbox/api/services/bundle/v1"
	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "bundle",
		Requires: []plugin.Type{
			cplugins.EventPlugin,
			cplugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			bundleDir := ic.Properties[plugins.PropertyBundleDir]
			if err := os.MkdirAll(bundleDir, 0755); err != nil {
				return nil, err
			}
			return &service{
				bundleRoot: bundleDir,
			}, nil
		},
	})
}

type service struct {
	bundleRoot string
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCBundleService(server, s)
	return nil
}

func (s *service) Create(ctx context.Context, r *api.CreateRequest) (*api.CreateResponse, error) {
	d := filepath.Join(s.bundleRoot, r.ID)
	if err := os.Mkdir(d, 0755); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	log.G(ctx).Infof("Creating bundle at %s", d)
	if err := os.Mkdir(filepath.Join(d, "rootfs"), 0755); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	for f, b := range r.Files {
		if err := os.WriteFile(filepath.Join(d, f), b, 0644); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
	}
	return &api.CreateResponse{
		Bundle: d,
	}, nil
}
