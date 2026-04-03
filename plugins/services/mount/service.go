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

package mount

import (
	"context"
	"fmt"
	"os"

	ctrMount "github.com/containerd/containerd/v2/core/mount"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	api "github.com/containerd/nerdbox/api/services/mount/v1"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "mount",
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			return &service{}, nil
		},
	})
}

type service struct{}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCMountService(server, s)
	return nil
}

func (s *service) MountAll(ctx context.Context, r *api.MountAllRequest) (*api.MountAllResponse, error) {
	for _, m := range r.Mounts {
		log.G(ctx).WithFields(log.Fields{
			"type":    m.Type,
			"source":  m.Source,
			"target":  m.Target,
			"options": m.Options,
		}).Info("mounting filesystem")

		if err := os.MkdirAll(m.Target, 0700); err != nil {
			return nil, errgrpc.ToGRPC(fmt.Errorf("failed to create mount target directory %s: %w", m.Target, err))
		}

		if err := ctrMount.All([]ctrMount.Mount{{
			Type:    m.Type,
			Source:  m.Source,
			Target:  m.Target,
			Options: m.Options,
		}}, "/"); err != nil {
			return nil, errgrpc.ToGRPC(fmt.Errorf("failed to mount %s at %s: %w", m.Source, m.Target, err))
		}
	}
	return &api.MountAllResponse{}, nil
}

func (s *service) Unmount(ctx context.Context, r *api.UnmountRequest) (*api.UnmountResponse, error) {
	log.G(ctx).WithField("target", r.Target).Info("unmounting filesystem")

	if err := ctrMount.Unmount(r.Target, 0); err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("failed to unmount %s: %w", r.Target, err))
	}
	return &api.UnmountResponse{}, nil
}
