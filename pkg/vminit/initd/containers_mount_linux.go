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

package initd

import (
	"context"
	"os"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
)

// mountContainersFS attempts to mount the "containers" virtiofs share at
// /run/containers. This share is added by the host shim when running in
// sandbox mode (CreateSandbox/StartSandbox) and exposes the assembled rootfs
// for each container under /run/containers/<id>/rootfs.
//
// /run is a tmpfs in the guest so the mount point directory can always be
// created, even though the erofs base rootfs is read-only.
//
// On the legacy single-container path the "containers" virtiofs tag is not
// registered by the host, so the mount will fail.  We log that at debug
// level and continue — the legacy path does not use this mount.
func mountContainersFS() {
	target := "/run/containers"

	// /run is a tmpfs so MkdirAll always succeeds here.
	if err := os.MkdirAll(target, 0o755); err != nil {
		log.G(context.Background()).WithError(err).Debug("failed to create /run/containers mountpoint")
		return
	}

	err := mount.All([]mount.Mount{{
		Type:   "virtiofs",
		Source: "containers",
		Target: target,
	}}, "/")
	if err != nil {
		// Expected on the legacy single-container path.
		log.G(context.Background()).WithError(err).Debug("containers virtiofs share not available (expected on single-container path)")
	}
}
