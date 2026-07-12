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

	"github.com/containerd/nerdbox/internal/podnetns"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// sharedNamespacesFunc is called by sanitizeNamespaces, at most once, only
// if a container's spec actually requests IPC or PID namespace sharing.
// It returns the guest paths of the sandbox's shared IPC and PID
// namespaces, creating them on first use — see internal/shim/task/podns.go
// for the concrete implementation (a lazily-called, memoized guest RPC).
type sharedNamespacesFunc func(ctx context.Context) (ipcPath, pidPath string, err error)

// sanitizeNamespaces is a bundle.Transformer for sandbox member containers.
// It has two jobs:
//
//  1. Strip host paths from the incoming OCI spec's Linux namespaces. In
//     production CRI, a member container's spec sets the network, IPC,
//     UTS, and (for pod- or node-level PID sharing) PID namespace entries'
//     Path to a host path (e.g. "/proc/<sandboxPid>/ns/net" —
//     containerd's WithPodNamespaces), since that is meaningful to a normal
//     (non-VM) OCI runtime running directly on the host. Copied verbatim
//     into the guest, that path is meaningless (or, if it happens to collide
//     with a real guest path, actively wrong) — the guest is a different
//     kernel with an unrelated PID/namespace space entirely.
//
//  2. Ensure member containers of the same sandbox share the namespaces
//     CRI actually asked them to share, using guest-side equivalents:
//
//     - Network: the shared, per-sandbox guest namespace at
//     podnetns.Path — created once at vminitd startup — so that every
//     default (no dedicated NIC annotation) member container shares one
//     guest network namespace. This intentionally does not affect host
//     reachability via TSI, which is not scoped by guest network
//     namespaces at all — see "TSI ignores guest-internal network
//     namespaces" in docs/sandbox-architecture.md. Its purpose is giving
//     member containers a shared L2/L3 view of each other, not host
//     isolation.
//
//     - IPC and PID: CRI's WithPodNamespaces sets a host Path on the IPC
//     namespace entry unconditionally (Kubernetes shares pod IPC by
//     default), and on the PID namespace entry whenever the pod's PID
//     sharing mode isn't NamespaceMode_CONTAINER (covering both
//     NamespaceMode_POD, e.g. shareProcessNamespace: true, and
//     NamespaceMode_NODE, e.g. hostPID: true). Since the shim reports its
//     own host PID as the sandbox's PID for both of these modes (there is
//     no guest-side "true host" to distinguish them by), this shim
//     deliberately does not try to tell hostPID/HostIPC apart from
//     PID/IPC-shared-within-the-pod: any non-empty incoming Path on
//     either namespace type is treated as "share within this pod" and
//     redirected to the pod's shared guest namespace (fetched lazily via
//     getSharedNS, since — unlike the network namespace — creating the
//     shared PID namespace needs a real anchor process; see
//     internal/vminit/podns and internal/vminit/podpause). A container
//     with no such entry at all (NamespaceMode_CONTAINER, the default)
//     keeps its own, independent namespace: getSharedNS is never called,
//     so a pod that never asks for PID/IPC sharing never pays for it.
//
// hasDedicatedNIC should be true when the container has its own
// annotation-driven virtio-NIC network configured (ctrNetConfig.Networks is
// non-empty). Such a container keeps its own, separate guest network
// namespace (crun's default: an empty-Path network namespace entry, which
// asks crun to create a fresh one) rather than joining the shared pod
// namespace, so per-container NIC/veth wiring in
// internal/vminit/ctrnetworking (which assumes each such container owns its
// namespace) is unaffected.
func sanitizeNamespaces(ctx context.Context, b *bundle.Bundle, hasDedicatedNIC bool, getSharedNS sharedNamespacesFunc) error {
	if b.Spec.Linux == nil {
		return nil
	}

	foundNetworkNS := false
	for i, ns := range b.Spec.Linux.Namespaces {
		switch ns.Type {
		case specs.NetworkNamespace:
			foundNetworkNS = true
			if !hasDedicatedNIC {
				b.Spec.Linux.Namespaces[i].Path = podnetns.Path
			} else {
				b.Spec.Linux.Namespaces[i].Path = ""
			}
		case specs.IPCNamespace:
			if ns.Path == "" {
				continue
			}
			ipcPath, _, err := getSharedNS(ctx)
			if err != nil {
				return fmt.Errorf("get shared ipc namespace: %w", err)
			}
			b.Spec.Linux.Namespaces[i].Path = ipcPath
		case specs.PIDNamespace:
			if ns.Path == "" {
				continue
			}
			_, pidPath, err := getSharedNS(ctx)
			if err != nil {
				return fmt.Errorf("get shared pid namespace: %w", err)
			}
			b.Spec.Linux.Namespaces[i].Path = pidPath
		default:
			// No other namespace type ever has a valid host Path in the
			// guest.
			b.Spec.Linux.Namespaces[i].Path = ""
		}
	}

	if !foundNetworkNS && !hasDedicatedNIC {
		b.Spec.Linux.Namespaces = append(b.Spec.Linux.Namespaces, specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: podnetns.Path,
		})
	}

	return nil
}
