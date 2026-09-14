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

// Package vm provides a vm backed sandbox implementation
package vm

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/pkg/vm"
	"github.com/containerd/ttrpc"
)

func NewVMSandbox(vmm vm.Manager) sandbox.Sandbox {
	return &localsandbox{
		vmm: vmm,
	}
}

type localsandbox struct {
	mu       sync.Mutex
	vmm      vm.Manager
	instance vm.Instance
	// stopping is set while a Stop call is tearing down instance. It
	// serializes concurrent Stop calls against each other and makes
	// Client/StartStream fail fast instead of racing a call against an
	// in-flight Shutdown.
	stopping bool
}

// diskReserver is a package-private optional interface for vm.Manager
// implementations that pre-attach virtio-block devices before container disks.
type diskReserver interface {
	ReservedDisks() int
}

func (s *localsandbox) ReservedDisks() int {
	if dr, ok := s.vmm.(diskReserver); ok {
		return dr.ReservedDisks()
	}
	return 0
}

func (s *localsandbox) Start(ctx context.Context, opts ...sandbox.Opt) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.instance != nil {
		return fmt.Errorf("sandbox is already started: %w", errdefs.ErrFailedPrecondition)
	}

	var o sandbox.Options
	for _, opt := range opts {
		opt(&o)
	}

	if o.StateDir == "" {
		return fmt.Errorf("state directory is required: %w", errdefs.ErrInvalidArgument)
	}

	vmi, err := s.vmm.NewInstance(ctx, o.StateDir)
	if err != nil {
		return err
	}
	vmiStarted := false
	defer func() {
		if !vmiStarted {
			_ = vmi.Shutdown(ctx)
		}
	}()

	for _, d := range o.Disks {
		var mountOpts []vm.MountOpt
		if d.Flags&sandbox.DiskFlagReadonly != 0 {
			mountOpts = append(mountOpts, vm.WithReadOnly())
		}
		if d.Flags&sandbox.DiskFlagVMDK != 0 {
			mountOpts = append(mountOpts, vm.WithVmdk())
		}
		if err := vmi.AddDisk(ctx, d.BlockID, d.MountPath, mountOpts...); err != nil {
			return err
		}
	}

	for _, fs := range o.Filesystems {
		var mountOpts []vm.MountOpt
		if fs.Readonly {
			mountOpts = append(mountOpts, vm.WithReadOnly())
		}
		if err := vmi.AddFS(ctx, fs.Tag, fs.MountPath, mountOpts...); err != nil {
			return err
		}
	}

	for _, n := range o.NICs {
		nicOpts := []vm.NetworkOpt{
			vm.WithNICFeatures(n.Features),
			vm.WithNICFlags(n.Flags),
		}
		if err := vmi.AddNIC(ctx, n.Endpoint, n.MAC, vm.NetworkMode(n.Mode), nicOpts...); err != nil {
			return err
		}
	}

	if o.CPU > 0 && o.Memory > 0 {
		if err := vmi.SetCPUAndMemory(ctx, o.CPU, o.Memory); err != nil {
			return err
		}
	} else if o.CPU > 0 || o.Memory > 0 {
		return fmt.Errorf("both CPU and Memory must be set: %w", errdefs.ErrInvalidArgument)
	}

	var startOpts []vm.StartOpt
	if len(o.InitArgs) > 0 {
		startOpts = append(startOpts, vm.WithInitArgs(o.InitArgs...))
	}

	if err := vmi.Start(ctx, startOpts...); err != nil {
		return err
	}

	vmiStarted = true
	s.instance = vmi
	return nil
}

func (s *localsandbox) Stop(ctx context.Context) error {
	instance, err := s.beginStopping()
	if err != nil {
		return err
	}

	stopped := false
	defer func() {
		s.mu.Lock()
		if stopped {
			// Stopped successful so clear vm instance.
			s.instance = nil
		}
		s.stopping = false
		s.mu.Unlock()
	}()

	if err := instance.Shutdown(ctx); err != nil {
		return err
	}

	stopped = true
	return nil
}

func (s *localsandbox) Client() (*ttrpc.Client, error) {
	instance, err := s.activeInstance()
	if err != nil {
		return nil, err
	}

	// A live instance can still hand back a nil client if a previous Stop
	// call failed partway through tearing it down (e.g. the underlying
	// Shutdown cleared its client but then failed on a later step, so
	// s.instance was deliberately left set for a retry). Surface that as
	// an unavailable error instead of a nil client with a nil error.
	client := instance.Client()
	if client == nil {
		return nil, errdefs.ErrUnavailable.WithMessage("sandbox client is unavailable")
	}

	return client, nil
}

func (s *localsandbox) StartStream(ctx context.Context, streamID string) (net.Conn, error) {
	instance, err := s.activeInstance()
	if err != nil {
		return nil, err
	}

	return instance.StartStream(ctx, streamID)
}

// beginStopping validates that the sandbox can be stopped, marks it
// stopping, and hands back the instance that is to be shut down.
func (s *localsandbox) beginStopping() (vm.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.instance == nil {
		return nil, errdefs.ErrFailedPrecondition.WithMessage("sandbox must be started")
	}
	if s.stopping {
		return nil, errdefs.ErrFailedPrecondition.WithMessage("sandbox is already stopping")
	}
	s.stopping = true
	return s.instance, nil
}

// activeInstance hands back an instance that has been started and is
// not begun any shutdown process.
func (s *localsandbox) activeInstance() (vm.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.instance == nil {
		return nil, errdefs.ErrFailedPrecondition.WithMessage("sandbox must be started")
	}
	if s.stopping {
		return nil, errdefs.ErrFailedPrecondition.WithMessage("sandbox is stopping")
	}

	return s.instance, nil
}
