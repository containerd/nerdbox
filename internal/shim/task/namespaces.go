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
	"sync"

	"github.com/containerd/ttrpc"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	nsAPI "github.com/containerd/nerdbox/api/services/namespaces/v1"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// sharedNamespacesFunc returns the guest paths of the sandbox's shared
// namespaces of the requested types, creating them on first use. It is called
// by sanitizeNamespaces at most once per container, and only if that
// container's spec actually asks to share something.
type sharedNamespacesFunc func(ctx context.Context, types []nsAPI.NamespaceType) (map[nsAPI.NamespaceType]string, error)

// sharedNamespaces calls the guest's NamespaceManager.Create the first time
// it is needed and memoizes the result per namespace type. A value is created
// fresh per Task.Create call (see createSandboxedContainer), so a container
// that shares nothing never triggers the guest RPC at all — and therefore
// never causes the guest to create a namespace, or to spawn the PID
// namespace's anchor process, on its behalf.
type sharedNamespaces struct {
	client    *ttrpc.Client // vminitd's TTRPC connection
	sandboxID string        // namespace group id

	mu    sync.Mutex
	paths map[nsAPI.NamespaceType]string
}

// get implements sharedNamespacesFunc. Types already fetched are served from
// the memo; only the remainder is requested from the guest.
func (n *sharedNamespaces) get(ctx context.Context, types []nsAPI.NamespaceType) (map[nsAPI.NamespaceType]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	var missing []nsAPI.NamespaceType
	for _, t := range types {
		if _, ok := n.paths[t]; !ok {
			missing = append(missing, t)
		}
	}

	if len(missing) > 0 {
		c := nsAPI.NewTTRPCNamespaceManagerClient(n.client)
		resp, err := c.Create(ctx, &nsAPI.CreateRequest{
			ID:    n.sandboxID,
			Types: missing,
		})
		if err != nil {
			return nil, fmt.Errorf("guest namespace create: %w", err)
		}
		if n.paths == nil {
			n.paths = make(map[nsAPI.NamespaceType]string, len(missing))
		}
		for _, ns := range resp.GetNamespaces() {
			n.paths[ns.GetType()] = ns.GetPath()
		}
	}

	out := make(map[nsAPI.NamespaceType]string, len(types))
	for _, t := range types {
		path, ok := n.paths[t]
		if !ok {
			return nil, fmt.Errorf("guest did not return a path for namespace type %q", t)
		}
		out[t] = path
	}
	return out, nil
}

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
//     obtained from getSharedNS.
//
// CRI's WithPodNamespaces sets a host Path on the IPC namespace entry
// unconditionally (Kubernetes shares pod IPC by default), and on the PID
// namespace entry whenever the pod's PID sharing mode isn't
// NamespaceMode_CONTAINER (covering both NamespaceMode_POD, e.g.
// shareProcessNamespace: true, and NamespaceMode_NODE, e.g. hostPID: true).
// Since the shim reports its own host PID as the sandbox's PID for both of
// those modes, there is no data in the request that would let it tell them
// apart, so any non-empty incoming Path on either type is treated as "share
// within this sandbox". A container with no such entry at all
// (NamespaceMode_CONTAINER, the default) keeps its own independent namespace.
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
//   - hasSandboxNIC: the VM has an interface, so containers join the
//     sandbox's shared namespace to get a common view of it.
//   - no NIC anywhere: there is no in-guest networking for a namespace to
//     scope, so the entry is dropped entirely, leaving the container in the
//     VM's own network namespace. Since the VM is per-sandbox that still gives
//     the containers of one sandbox a shared view of each other, including
//     loopback, but costs nothing to set up and cannot be mistaken for
//     isolation it does not provide. Container traffic in this configuration
//     reaches the host by other means (the guest kernel proxying its IP
//     sockets), which no network namespace can scope in any case.
func sanitizeNamespaces(ctx context.Context, b *bundle.Bundle, hasContainerNIC, hasSandboxNIC bool, getSharedNS sharedNamespacesFunc) error {
	if b.Spec.Linux == nil {
		return nil
	}

	shareNetwork := !hasContainerNIC && hasSandboxNIC
	dropNetwork := !hasContainerNIC && !hasSandboxNIC

	// First pass: work out which shared namespaces this container needs, so
	// they can all be requested from the guest in one call.
	wantNetwork := shareNetwork
	var (
		wantIPC bool
		wantPID bool
	)
	foundNetworkNS := false
	for _, ns := range b.Spec.Linux.Namespaces {
		switch ns.Type {
		case specs.NetworkNamespace:
			foundNetworkNS = true
		case specs.IPCNamespace:
			wantIPC = wantIPC || ns.Path != ""
		case specs.PIDNamespace:
			wantPID = wantPID || ns.Path != ""
		}
	}

	var types []nsAPI.NamespaceType
	if wantNetwork {
		types = append(types, nsAPI.NamespaceType_NAMESPACE_TYPE_NETWORK)
	}
	if wantIPC {
		types = append(types, nsAPI.NamespaceType_NAMESPACE_TYPE_IPC)
	}
	if wantPID {
		types = append(types, nsAPI.NamespaceType_NAMESPACE_TYPE_PID)
	}

	var paths map[nsAPI.NamespaceType]string
	if len(types) > 0 {
		var err error
		if paths, err = getSharedNS(ctx, types); err != nil {
			return fmt.Errorf("get shared namespaces: %w", err)
		}
	}

	// Second pass: rewrite the spec. Built as a new slice because the network
	// namespace entry is dropped outright in the no-guest-networking case.
	out := make([]specs.LinuxNamespace, 0, len(b.Spec.Linux.Namespaces)+1)
	for _, ns := range b.Spec.Linux.Namespaces {
		switch ns.Type {
		case specs.NetworkNamespace:
			switch {
			case dropNetwork:
				continue
			case shareNetwork:
				ns.Path = paths[nsAPI.NamespaceType_NAMESPACE_TYPE_NETWORK]
			default:
				ns.Path = ""
			}
		case specs.IPCNamespace:
			if ns.Path != "" {
				ns.Path = paths[nsAPI.NamespaceType_NAMESPACE_TYPE_IPC]
			}
		case specs.PIDNamespace:
			if ns.Path != "" {
				ns.Path = paths[nsAPI.NamespaceType_NAMESPACE_TYPE_PID]
			}
		default:
			// No other namespace type ever has a valid host Path in the
			// guest.
			ns.Path = ""
		}
		out = append(out, ns)
	}

	if !foundNetworkNS && shareNetwork {
		out = append(out, specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: paths[nsAPI.NamespaceType_NAMESPACE_TYPE_NETWORK],
		})
	}

	if len(out) == 0 {
		out = nil
	}
	b.Spec.Linux.Namespaces = out

	return nil
}
