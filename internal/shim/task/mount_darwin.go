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

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/nerdbox/internal/vm"
)

func setupMounts(ctx context.Context, vmi vm.Instance, id string, ms []*types.Mount, _, _ string) ([]*types.Mount, error) {
	mounts, err := transformMounts(ctx, vmi, id, ms)
	if err != nil {
		if errdefs.IsNotImplemented(err) {
			log.G(ctx).WithField("mounts", ms).Warnf("mount transformation was not allowed, passing mounts through")
		} else {
			return nil, err
		}
	}
	return mounts, nil
}
