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
	"slices"

	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// podSandboxConfig unmarshals a sandbox's CreateSandboxRequest.Options into
// a CRI PodSandboxConfig. opts is exactly what SandboxService.Options()
// returns: nil for a sandbox created without one (a legacy/non-CRI
// caller), otherwise the opaque payload the sandbox package intentionally
// does not interpret itself (see that field's doc comment).
//
// Returns (nil, nil) for a nil opts — this is the common case for anything
// that isn't real CRI (e.g. shimtest's sandbox suite, `ctr` sandboxes) and
// must not be treated as an error. A non-nil error means opts was present
// but did not unmarshal as a PodSandboxConfig; callers should treat that
// as non-fatal too (log and continue without pod config) since a shim
// must never fail Task.Create over an optional, best-effort feature.
func podSandboxConfig(opts *anypb.Any) (*criapi.PodSandboxConfig, error) {
	if opts == nil {
		return nil, nil
	}
	var cfg criapi.PodSandboxConfig
	if err := typeurl.UnmarshalTo(opts, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal sandbox options as PodSandboxConfig: %w", err)
	}
	return &cfg, nil
}

// addHostname sets the container's hostname to match the pod's, mirroring
// what CRI's podsandbox controller does for the podsandbox path
// (Controller.setupSandboxFiles writing an /etc/hostname bind-mounted into
// every member container — internal/cri/server/podsandbox/sandbox_run_linux.go
// upstream). The shim sandboxer path this package implements gets no such
// file from containerd (only the podsandbox controller creates one), so
// the shim must generate it itself from the pod config it already has.
//
// hostname empty is a no-op: crun/the guest kernel's own default applies.
func addHostname(_ context.Context, b *bundle.Bundle, hostname string) error {
	if hostname == "" {
		return nil
	}

	// The OCI runtime spec's own Hostname field is what actually sets the
	// container's UTS hostname (crun calls sethostname() after
	// establishing the UTS namespace). Setting this is enough on its own
	// for anything using gethostname(2)/uname(2); the /etc/hostname file
	// below additionally covers programs that read the file directly.
	b.Spec.Hostname = hostname

	if slices.ContainsFunc(b.Spec.Mounts, func(m specs.Mount) bool {
		return m.Destination == "/etc/hostname"
	}) {
		return nil
	}

	b.AddExtraFile("hostname", []byte(hostname+"\n"))
	b.Spec.Mounts = append(b.Spec.Mounts, specs.Mount{
		Destination: "/etc/hostname",
		Type:        "bind",
		Source:      "hostname",
		Options:     []string{"rbind", "rprivate"},
	})
	return nil
}

// addSysctls merges the pod's CRI sysctls (PodSandboxConfig.Linux.Sysctls
// — CRI only carries sysctls at the pod level, not per-container) into
// the container's OCI spec, which crun applies inside the container's
// namespaces at start. Existing spec.Linux.Sysctl entries win on key
// collision (an explicit per-container value, however it got there, is
// assumed more specific than the pod default).
//
// A nil/empty sysctls map is a no-op.
func addSysctls(_ context.Context, b *bundle.Bundle, sysctls map[string]string) error {
	if len(sysctls) == 0 {
		return nil
	}
	if b.Spec.Linux == nil {
		b.Spec.Linux = &specs.Linux{}
	}
	if b.Spec.Linux.Sysctl == nil {
		b.Spec.Linux.Sysctl = make(map[string]string, len(sysctls))
	}
	for k, v := range sysctls {
		if _, exists := b.Spec.Linux.Sysctl[k]; exists {
			continue
		}
		b.Spec.Linux.Sysctl[k] = v
	}
	return nil
}
