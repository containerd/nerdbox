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

// Package podnetns holds the well-known, guest-side identity of the shared
// network namespace that all member containers of a sandbox join by
// default. It is a plain constant (no platform-specific logic) so that both
// the host-side bundle transformer (internal/shim/task) and the guest-side
// namespace creator (internal/vminit/podnetns) can agree on the same path
// without importing each other.
//
// This is distinct from, and unrelated to, the host-side "network sandbox"
// (the pod netns pinned by the shim and entered by the libkrun executor
// thread — see docs/sandbox-architecture.md, Layer 1). Path identifies a
// namespace that exists purely inside the guest kernel; it has no bearing
// on TSI/host reachability, which is scoped entirely by the host-side
// mechanism (see the "TSI ignores guest-internal network namespaces"
// section of that same doc). Its purpose is solely to give member
// containers of one sandbox a shared L2/L3 view of each other (so
// localhost-style and veth/bridge container-to-container traffic behaves
// like a real pod), not to isolate them from the host.
package podnetns

// Name is the name of the persistent guest network namespace, as passed to
// (github.com/vishvananda/netns).NewNamed.
const Name = "pod"

// Path is the well-known guest-side bind-mount path for the persistent,
// shared network namespace created at vminitd startup (see
// internal/vminit/podnetns.Create). A sandbox member container's OCI spec
// network namespace Path is rewritten to this value (see
// internal/shim/task's netns bundle transformer) so that all member
// containers of the same sandbox land in the same guest network namespace,
// regardless of what — if anything — the incoming spec's namespace Path
// originally pointed to on the host.
const Path = "/run/netns/" + Name
