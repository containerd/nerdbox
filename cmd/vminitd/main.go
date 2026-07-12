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

package main

import (
	"context"
	"os"

	"github.com/containerd/log"

	"github.com/containerd/nerdbox/internal/vminit/podpause"
	"github.com/containerd/nerdbox/pkg/vminit/initd"

	_ "github.com/containerd/nerdbox/plugins/services/bundle"
	_ "github.com/containerd/nerdbox/plugins/services/mount"
	_ "github.com/containerd/nerdbox/plugins/services/podns"
	_ "github.com/containerd/nerdbox/plugins/services/system"
	_ "github.com/containerd/nerdbox/plugins/services/transfer"

	_ "github.com/containerd/nerdbox/plugins/vminit/events"
	_ "github.com/containerd/nerdbox/plugins/vminit/socketforward"
	_ "github.com/containerd/nerdbox/plugins/vminit/streaming"
	_ "github.com/containerd/nerdbox/plugins/vminit/task"
)

func main() {
	// Hidden subcommand: vminitd re-execs itself as "pod-pause" (see
	// internal/vminit/podns.createPIDAnchor) to anchor a sandbox's shared
	// PID namespace. This must be checked before any of the normal
	// vminitd startup/flag-parsing logic runs.
	if len(os.Args) > 1 && os.Args[1] == "pod-pause" {
		podpause.Run()
		return
	}

	if err := initd.Run(context.Background()); err != nil {
		log.G(context.Background()).WithError(err).Error("vminitd exited")
		os.Exit(1)
	}
}
