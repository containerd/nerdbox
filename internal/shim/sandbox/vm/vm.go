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
	"github.com/containerd/nerdbox/internal/vm"
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
}

func (s *localsandbox) Start(ctx context.Context, opts ...sandbox.Opt) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.instance != nil {
		return errdefs.ErrFailedPrecondition
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
		if err := vmi.AddNIC(ctx, n.Endpoint, n.MAC, vm.NetworkMode(n.Mode), n.Features, n.Flags); err != nil {
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

	s.instance = vmi
	return nil
}

func (s *localsandbox) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.instance == nil {
		return errdefs.ErrFailedPrecondition
	}

	if err := s.instance.Shutdown(ctx); err != nil {
		return err
	}

	s.instance = nil
	return nil
}

func (s *localsandbox) Client() (*ttrpc.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.instance == nil {
		return nil, errdefs.ErrFailedPrecondition
	}

	return s.instance.Client(), nil
}

func (s *localsandbox) StartStream(ctx context.Context) (uint32, net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.instance == nil {
		return 0, nil, errdefs.ErrFailedPrecondition
	}

	return s.instance.StartStream(ctx)
}
