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

	specs "github.com/opencontainers/runtime-spec/specs-go"

	srAPI "github.com/containerd/nerdbox/api/services/sharedresources/v1"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// sanitizeNamespaces is a bundle.Transformer for sandbox member containers.
// It has two jobs:
//
//  1. Strip host paths from the incoming OCI spec's Linux namespaces. In
//     production CRI, a member container's spec sets the network, IPC, UTS,
//     and (for pod- or node-level PID sharing) PID namespace entries' Path to
//     a host path (e.g. "/proc/<sandboxPid>/ns/net" — containerd's
//     WithPodNamespaces), since that is meaningful to a normal (non-VM) OCI
//     runtime running directly on the host. Copied verbatim into the guest,
//     that path is meaningless (or, if it happens to collide with a real
//     guest path, actively wrong) — the guest is a different kernel with an
//     unrelated PID/namespace space entirely.
//
//  2. Ensure member containers of the same sandbox share the namespaces CRI
//     actually asked them to share, by substituting guest-side equivalents
//     obtained from getSharedResources.
//
// CRI's WithPodNamespaces sets a host Path on the IPC and UTS namespace
// entries unconditionally (Kubernetes shares both by default, and does not
// expose a per-pod option to turn either off), and on the PID namespace
// entry whenever the pod's PID sharing mode isn't NamespaceMode_CONTAINER
// (covering both NamespaceMode_POD, e.g. shareProcessNamespace: true, and
// NamespaceMode_NODE, e.g. hostPID: true). Since the shim reports its own
// host PID as the sandbox's PID for both of those modes, there is no data in
// the request that would let it tell them apart, so any non-empty incoming
// Path on any of these three types is treated as "share within this
// sandbox". A container with no such entry at all (NamespaceMode_CONTAINER,
// the default, applicable to PID only) keeps its own independent namespace.
//
// Sharing a UTS namespace needs no extra coordination for the hostname
// itself: an OCI runtime setting Spec.Hostname while joining an existing
// (rather than freshly created) UTS namespace calls sethostname(2) after
// joining it — updating the namespace, and so every container sharing it,
// rather than erroring — and leaves it alone when Hostname is empty. Every
// member container of a pod already carries the same CRI-provided hostname
// on its own spec, so this "last write wins, empty means no opinion"
// behavior is exactly what is wanted, entirely for free.
//
// Namespaces are requested from the guest in a single call, and only the
// types this container actually needs are asked for. That matters for the PID
// namespace in particular, which the guest can only provide by spawning a
// persistent anchor process.
//
// A network namespace exists to scope in-guest networking, and a virtio-net
// interface is what creates that, so NIC presence alone decides the outcome:
//
//   - hasContainerNIC: the container has its own annotation-driven NIC
//     (ctrNetConfig.Networks is non-empty), so it keeps a namespace of its own
//     — an empty Path, which asks the runtime to create a fresh one. The
//     per-container NIC/veth wiring in internal/vminit/ctrnetworking assumes
//     each such container owns its namespace, so it must not be put in a
//     shared one.
//   - no container NIC: there is no in-guest networking for a namespace to
//     scope, so the entry is dropped entirely, leaving the container in the
//     VM's own network namespace. Container traffic in this configuration
//     reaches the host by other means (the guest kernel proxying its IP
//     sockets, i.e. TSI), which no network namespace can scope in any case.
//
// A shared guest network namespace for the case where the sandbox itself has
// a NIC but this container does not is deliberately not implemented: TSI
// hijacks a socket() call on address family alone, before any namespace or
// routing decision, so it is not scoped by a guest network namespace at all,
// and a real virtio-net interface is only ever plumbed into the VM's own
// initial network namespace (see internal/vminit/vmnetworking.SetupVM) — a
// second, separate network namespace created for member containers would
// contain nothing but loopback, cut those containers off from the sandbox's
// actual NIC entirely, and is not exercised by any test. If per-container
// sharing of a sandbox-level NIC is wanted in the future, the interface (or
// a veth peer of it) needs to be plumbed into the shared namespace itself,
// not just have containers join an empty one.
func sanitizeNamespaces(ctx context.Context, b *bundle.Bundle, hasContainerNIC bool, getSharedResources sharedResourceFunc) error {
	if b.Spec.Linux == nil {
		return nil
	}

	dropNetwork := !hasContainerNIC

	// First pass: work out which shared namespaces this container needs, so
	// they can all be requested from the guest in one call. The network
	// namespace is never one of them — see the doc comment above for why
	// there is no shared-network-namespace case at all.
	var (
		wantIPC bool
		wantUTS bool
		wantPID bool
	)
	for _, ns := range b.Spec.Linux.Namespaces {
		switch ns.Type {
		case specs.IPCNamespace:
			wantIPC = wantIPC || ns.Path != ""
		case specs.UTSNamespace:
			wantUTS = wantUTS || ns.Path != ""
		case specs.PIDNamespace:
			wantPID = wantPID || ns.Path != ""
		}
	}

	var types []srAPI.Type
	if wantIPC {
		types = append(types, srAPI.Type_TYPE_NAMESPACE_IPC)
	}
	if wantUTS {
		types = append(types, srAPI.Type_TYPE_NAMESPACE_UTS)
	}
	if wantPID {
		types = append(types, srAPI.Type_TYPE_NAMESPACE_PID)
	}

	var paths map[srAPI.Type]string
	if len(types) > 0 {
		var err error
		if paths, err = getSharedResources(ctx, types); err != nil {
			return fmt.Errorf("get shared namespaces: %w", err)
		}
	}

	// Second pass: rewrite the spec. Built as a new slice because the network
	// namespace entry is dropped outright when this container has no NIC of
	// its own.
	out := make([]specs.LinuxNamespace, 0, len(b.Spec.Linux.Namespaces))
	for _, ns := range b.Spec.Linux.Namespaces {
		switch ns.Type {
		case specs.NetworkNamespace:
			if dropNetwork {
				continue
			}
			// hasContainerNIC: keep the entry but clear any host Path, so
			// the guest runtime creates this container a fresh namespace
			// of its own for internal/vminit/ctrnetworking's veth wiring
			// to attach to.
			ns.Path = ""
		case specs.IPCNamespace:
			if ns.Path != "" {
				ns.Path = paths[srAPI.Type_TYPE_NAMESPACE_IPC]
			}
		case specs.UTSNamespace:
			if ns.Path != "" {
				ns.Path = paths[srAPI.Type_TYPE_NAMESPACE_UTS]
			}
		case specs.PIDNamespace:
			if ns.Path != "" {
				ns.Path = paths[srAPI.Type_TYPE_NAMESPACE_PID]
			}
		default:
			// No other namespace type ever has a valid host Path in the
			// guest.
			ns.Path = ""
		}
		out = append(out, ns)
	}

	if len(out) == 0 {
		out = nil
	}
	b.Spec.Linux.Namespaces = out

	return nil
}
