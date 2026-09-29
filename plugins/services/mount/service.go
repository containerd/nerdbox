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
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"

	ctrMount "github.com/containerd/containerd/v2/core/mount"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/moby/sys/mountinfo"

	api "github.com/containerd/nerdbox/api/services/mount/v1"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "mount",
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			return &service{mounted: mountinfo.Mounted, doMount: doMount}, nil
		},
	})
}

type service struct {
	mu     sync.Mutex
	mounts []*api.MountSpec // in-VM mounts, in mount order

	// mounted reports whether path is currently a real mount point. It is
	// mountinfo.Mounted in production; tests substitute a fake to exercise
	// the bookkeeping-reconcile logic in MountAll without needing a
	// privileged real mount.
	mounted func(path string) (bool, error)

	// doMount creates m.Target and performs the real mount. It is doMount
	// in production; tests substitute a fake for the same reason as
	// mounted above.
	doMount func(m *api.MountSpec) error
}

// doMount creates the mount point directory and performs the real mount
// described by m.
func doMount(m *api.MountSpec) error {
	if err := os.MkdirAll(m.Target, 0700); err != nil {
		return fmt.Errorf("failed to create mount target directory %s: %w", m.Target, err)
	}

	if err := ctrMount.All([]ctrMount.Mount{{
		Type:    m.Type,
		Source:  m.Source,
		Target:  m.Target,
		Options: m.Options,
	}}, "/"); err != nil {
		return fmt.Errorf("failed to mount %s at %s: %w", m.Source, m.Target, err)
	}
	return nil
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCMountService(server, s)
	return nil
}

func (s *service) MountAll(ctx context.Context, r *api.MountAllRequest) (*api.MountAllResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, m := range r.Mounts {
		log.G(ctx).WithFields(log.Fields{
			"type":    m.Type,
			"source":  m.Source,
			"target":  m.Target,
			"options": m.Options,
		}).Info("mounting filesystem")

		i := slices.IndexFunc(s.mounts, func(e *api.MountSpec) bool { return e.Target == m.Target })
		if i >= 0 {
			// Bookkeeping alone is not enough to skip the mount: nothing
			// prevents the target from having been unmounted by other
			// means since it was recorded (e.g. a container's rootfs
			// cleanup racing a reused container ID, or the guest mount
			// service having restarted). Reconcile against the real
			// mount table before trusting the record, so a stale entry
			// cannot cause a container to silently start with the wrong
			// (or no) filesystem mounted at its target.
			mounted, err := s.mounted(m.Target)
			if err != nil && !os.IsNotExist(err) {
				return nil, errgrpc.ToGRPC(fmt.Errorf("check mount state of %s: %w", m.Target, err))
			}
			if mounted {
				if mountSpecsEqual(s.mounts[i], m) {
					log.G(ctx).WithField("target", m.Target).Debug("mount already exists with matching spec; skipping")
					continue
				}
				return nil, errgrpc.ToGRPC(fmt.Errorf("target %s already mounted with a different spec: %w", m.Target, errdefs.ErrAlreadyExists))
			}
			// The bookkeeping entry is stale: the target is not actually
			// mounted (or no longer exists) despite our record saying
			// otherwise. Drop it and fall through to mount fresh below.
			// This can never disturb another container's mounts: mount
			// targets are per-container by construction, so reconciling
			// this entry away never touches state any other container
			// depends on, and nothing here ever issues an unmount.
			log.G(ctx).WithField("target", m.Target).Warn("bookkeeping said this target was mounted, but it is not; remounting")
			s.mounts = slices.Delete(s.mounts, i, i+1)
		}

		if err := s.doMount(m); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}

		s.mounts = append(s.mounts, m)
	}
	return &api.MountAllResponse{}, nil
}

func (s *service) Unmount(ctx context.Context, r *api.UnmountRequest) (*api.UnmountResponse, error) {
	log.G(ctx).WithField("target", r.Target).Info("unmounting filesystem")

	s.mu.Lock()
	defer s.mu.Unlock()

	i := slices.IndexFunc(s.mounts, func(m *api.MountSpec) bool { return m.Target == r.Target })
	if i < 0 {
		return nil, errgrpc.ToGRPC(fmt.Errorf("cannot unmount %s: %w", r.Target, errdefs.ErrNotFound))
	}
	// ctrMount.Unmount already treats "not currently a mount point" as
	// success; also tolerate the target directory itself having been
	// removed already (e.g. by rootfs cleanup racing this call), so a
	// caller retrying a partially-failed teardown does not get stuck on
	// state that is already gone.
	if err := ctrMount.Unmount(r.Target, 0); err != nil && !os.IsNotExist(err) {
		return nil, errgrpc.ToGRPC(fmt.Errorf("failed to unmount %s: %w", r.Target, err))
	}
	s.mounts = slices.Delete(s.mounts, i, i+1)

	return &api.UnmountResponse{}, nil
}

func (s *service) UnmountAll(ctx context.Context, _ *api.UnmountAllRequest) (*api.UnmountAllResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Unmount in reverse order (deepest first).
	var errs []error
	for i := len(s.mounts) - 1; i >= 0; i-- {
		target := s.mounts[i].Target
		log.G(ctx).WithField("target", target).Info("unmounting filesystem")
		// See the comment in Unmount: tolerate the target already being
		// gone, on top of ctrMount.Unmount's own tolerance of "not
		// currently a mount point".
		if err := ctrMount.Unmount(target, 0); err != nil && !os.IsNotExist(err) {
			log.G(ctx).WithError(err).WithField("target", target).Warn("failed to unmount")
			errs = append(errs, fmt.Errorf("unmount %s: %w", target, err))
			continue
		}
		s.mounts = slices.Delete(s.mounts, i, i+1)
	}
	if len(errs) > 0 {
		return nil, errgrpc.ToGRPC(fmt.Errorf("unmount errors: %w", errors.Join(errs...)))
	}
	return &api.UnmountAllResponse{}, nil
}

func mountSpecsEqual(a, b *api.MountSpec) bool {
	return a.Type == b.Type &&
		a.Source == b.Source &&
		a.Target == b.Target &&
		slices.Equal(a.Options, b.Options)
}
