//go:build windows

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

package libkrun

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/containerd/nerdbox/internal/vm"
)

// NewManager returns a vm.Manager stub that returns ErrNotImplemented on Windows.
func NewManager() vm.Manager {
	return &vmManager{}
}

type vmManager struct{}

func (*vmManager) NewInstance(_ context.Context, _ string) (vm.Instance, error) {
	return nil, fmt.Errorf("libkrun is not supported on Windows: %w", errdefs.ErrNotImplemented)
}
