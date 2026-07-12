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

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/nerdbox/internal/podnetns"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// sanitizeNamespaces is a bundle.Transformer for sandbox member containers.
// It has two jobs:
//
//  1. Strip host paths from the incoming OCI spec's Linux namespaces. In
//     production CRI, a member container's spec sets the network namespace
//     entry's Path to a host path (e.g. "/proc/<sandboxPid>/ns/net" —
//     containerd's WithPodNamespaces), since that is meaningful to a normal
//     (non-VM) OCI runtime running directly on the host. Copied verbatim
//     into the guest, that path is meaningless (or, if it happens to collide
//     with a real guest path, actively wrong) — the guest is a different
//     kernel with an unrelated PID/namespace space entirely. The same
//     applies to a host Path on any other namespace type; none of them
//     survive the host-to-guest transition, so any non-empty Path is
//     cleared.
//
//  2. Ensure the container's network namespace is the shared, per-sandbox
//     guest namespace at podnetns.Path — created once at vminitd startup —
//     so that every default (no dedicated NIC annotation) member container
//     of the same sandbox shares one guest network namespace, the same way
//     containers of a real Kubernetes pod share the pod's network
//     namespace. This intentionally does not affect host reachability via
//     TSI, which is not scoped by guest network namespaces at all — see
//     "TSI ignores guest-internal network namespaces" in
//     docs/sandbox-architecture.md. Its purpose is giving member containers
//     a shared L2/L3 view of each other, not host isolation.
//
// hasDedicatedNIC should be true when the container has its own
// annotation-driven virtio-NIC network configured (ctrNetConfig.Networks is
// non-empty). Such a container keeps its own, separate guest network
// namespace (crun's default: an empty-Path network namespace entry, which
// asks crun to create a fresh one) rather than joining the shared pod
// namespace, so per-container NIC/veth wiring in
// internal/vminit/ctrnetworking (which assumes each such container owns its
// namespace) is unaffected.
func sanitizeNamespaces(_ context.Context, b *bundle.Bundle, hasDedicatedNIC bool) error {
	if b.Spec.Linux == nil {
		return nil
	}

	foundNetworkNS := false
	for i, ns := range b.Spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			foundNetworkNS = true
			if !hasDedicatedNIC {
				b.Spec.Linux.Namespaces[i].Path = podnetns.Path
			} else {
				b.Spec.Linux.Namespaces[i].Path = ""
			}
			continue
		}
		// No other namespace type ever has a valid host Path in the guest.
		b.Spec.Linux.Namespaces[i].Path = ""
	}

	if !foundNetworkNS && !hasDedicatedNIC {
		b.Spec.Linux.Namespaces = append(b.Spec.Linux.Namespaces, specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: podnetns.Path,
		})
	}

	return nil
}
