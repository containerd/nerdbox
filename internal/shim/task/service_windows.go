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

package task

import (
	"context"
	"os"

	"github.com/containerd/log"
)

// removeRootfsDir removes the rootfs directory from the bundle so that
// containerd's bundle cleanup doesn't attempt a bind filter unmount.
// On Windows, Unmount calls bindfilter.RemoveFileBinding which fails with
// ERROR_ACCESS_DENIED on directories that were never bind filter mounts
// (nerdbox uses VM-based virtio block devices instead). Removing the
// directory makes UnmountAll a no-op.
func removeRootfsDir(ctx context.Context) {
	if err := os.RemoveAll("rootfs"); err != nil {
		log.G(ctx).WithError(err).Warn("failed to remove rootfs directory during shutdown")
	}
}
