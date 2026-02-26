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

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"

	"github.com/containerd/nerdbox/internal/mountutil"
	"github.com/containerd/nerdbox/internal/shim/sandbox"
)

func setupMounts(ctx context.Context, id string, m []*types.Mount, rootfs, lmounts string) ([]*types.Mount, []sandbox.Opt, error) {
	// Handle mounts
	var sbOpts []sandbox.Opt

	if len(m) == 1 && (m[0].Type == "overlay" || m[0].Type == "bind") {
		tag := fmt.Sprintf("rootfs-%s", id)
		// virtiofs implementation has a limit of 36 characters for the tag
		if len(tag) > 36 {
			tag = tag[:36]
		}
		mnt := mount.Mount{
			Type:    m[0].Type,
			Source:  m[0].Source,
			Options: m[0].Options,
		}
		if err := mnt.Mount(rootfs); err != nil {
			return nil, nil, err
		}
		sbOpts = append(sbOpts, sandbox.WithFS(tag, rootfs, false))
		return []*types.Mount{{
			Type:   "virtiofs",
			Source: tag,
			// TODO: Translate the options
			//Options: m[0].Options,
		}}, sbOpts, nil
	} else if len(m) == 0 {
		tag := fmt.Sprintf("rootfs-%s", id)
		// virtiofs implementation has a limit of 36 characters for the tag
		if len(tag) > 36 {
			tag = tag[:36]
		}
		sbOpts = append(sbOpts, sandbox.WithFS(tag, rootfs, false))
		return []*types.Mount{{
			Type:   "virtiofs",
			Source: tag,
		}}, sbOpts, nil
	}
	mounts, opts, err := transformMounts(ctx, id, m)
	if err != nil && errdefs.IsNotImplemented(err) {
		if err := mountutil.All(ctx, rootfs, lmounts, m); err != nil {
			return nil, nil, err
		}

		// Fallback to original rootfs mount
		tag := fmt.Sprintf("rootfs-%s", id)
		// virtiofs implementation has a limit of 36 characters for the tag
		if len(tag) > 36 {
			tag = tag[:36]
		}
		sbOpts = append(sbOpts, sandbox.WithFS(tag, rootfs, false))
		return []*types.Mount{{
			Type:   "virtiofs",
			Source: tag,
		}}, sbOpts, nil
	}
	if len(opts) > 0 {
		sbOpts = append(sbOpts, opts...)
	}
	return mounts, sbOpts, err
}
