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

package main

import (
	"context"
	"os"

	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/log"

	"github.com/containerd/nerdbox/pkg/logging"
	"github.com/containerd/nerdbox/pkg/shim/manager"

	_ "github.com/containerd/nerdbox/plugins/sandbox"
	_ "github.com/containerd/nerdbox/plugins/shim/sandbox"
	_ "github.com/containerd/nerdbox/plugins/shim/streaming"
	_ "github.com/containerd/nerdbox/plugins/shim/task"
	_ "github.com/containerd/nerdbox/plugins/shim/transfer"
	_ "github.com/containerd/nerdbox/plugins/task"
	_ "github.com/containerd/nerdbox/plugins/vm/libkrun"
)

func init() {
	logging.SetupShimLog()
}

func main() {
	// Only ever set on the shim server child cloneMntNs itself launched
	// (see manager.MountNSIsolatedEnv), so this never runs for containerd's
	// own separate, direct "start"/"delete" invocations of this same
	// binary. Must happen before any container-related mount, so as early
	// in startup as possible.
	if os.Getenv(manager.MountNSIsolatedEnv) == "1" {
		if err := manager.IsolateMountPropagation(); err != nil {
			log.G(context.Background()).WithError(err).Warn(
				"failed to isolate shim mount namespace propagation; container rootfs mounts may leak into the host")
		}
	}

	shim.RunShim(context.Background(), manager.New("io.containerd.nerdbox.v1"),
		func(c *shim.Config) {
			c.NoSetupLogger = true
		},
	)
}
