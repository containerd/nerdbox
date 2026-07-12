// Copyright The containerd Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package task

import (
	"context"

	"github.com/containerd/log"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// SandboxStartOptions parses the sandbox OCI bundle at bundlePath to derive
// the VM start options: resources (CPU/mem), networking (NICs, init args),
// and resolv.conf injection. It is registered with the SandboxService as its
// StartOptionsFunc, allowing the sandbox service to boot the VM without
// importing the task package (avoiding a circular dependency).
//
// bundlePath is the path the containerd sandbox controller passed in
// CreateSandboxRequest.BundlePath. It may be the shim's working directory for
// the sandbox bundle.
func SandboxStartOptions(debug bool) sandbox.StartOptionsFunc {
	return func(ctx context.Context, bundlePath string) ([]sandbox.Opt, error) {
		var (
			nwpr        networksProvider
			resCfg      resourceConfig
			dumpInfoCfg dumpInfoConfig
		)

		_, err := bundle.Load(ctx, bundlePath,
			nwpr.FromBundle,
			resCfg.FromBundle,
			dumpInfoCfg.FromBundle,
			func(ctx context.Context, b *bundle.Bundle) error {
				return addResolvConf(ctx, b, len(nwpr.nws) == 0)
			},
		)
		if err != nil {
			// Sandbox bundle may be minimal (no config.json) — use defaults.
			log.G(ctx).WithError(err).Debug("sandbox bundle load failed; using resource defaults")
			return []sandbox.Opt{
				sandbox.WithResources(2, 2048),
			}, nil
		}

		var opts []sandbox.Opt
		opts = append(opts, resCfg.SandboxOpts()...)
		opts = append(opts, nwpr.SandboxOptions()...)
		opts = append(opts, dumpInfoCfg.SandboxOpts()...)
		if debug {
			opts = append(opts, sandbox.WithInitArgs("-debug"))
		}
		opts = append(opts, sandbox.WithInitArgs(nwpr.InitArgs()...))

		return opts, nil
	}
}
