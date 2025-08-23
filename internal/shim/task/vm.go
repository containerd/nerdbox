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

package task

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/containerd/ttrpc"

	"github.com/dmcgowan/nerdbox/internal/vm"
)

func (s *service) client() (*ttrpc.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return nil, fmt.Errorf("vm not created: %w", errdefs.ErrFailedPrecondition)
	}
	client := s.vm.Client()
	if client == nil {
		return nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}
	return client, nil
}

func (s *service) vmInstance(ctx context.Context, state string) (vm.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		var err error
		s.vm, err = s.vmm.NewInstance(ctx, state)
		if err != nil {
			return nil, err
		}
	}
	return s.vm, nil
}
