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

// Package podns holds the well-known, guest-side identity of the shared
// IPC and PID namespaces that member containers of a sandbox join when
// the pod's CRI NamespaceOptions request POD-level sharing. It is a plain
// constants package (no platform-specific logic) so that both the
// host-side bundle transformer (internal/shim/task) and the guest-side
// namespace creator (internal/vminit/podns) can agree on the same paths
// without importing each other.
//
// This mirrors internal/podnetns, which does the same thing for the
// shared network namespace; see that package's doc comment for why guest
// namespace sharing is unrelated to (and does not affect) TSI/host
// reachability. Unlike the network namespace, the shared PID namespace
// additionally requires a persistent anchor process (see
// internal/vminit/podpause) — a PID namespace has no content and is torn
// down the moment its PID 1 exits, unlike a network or IPC namespace,
// which can be anchored by a bind-mount alone.
package podns

// IPCPath is the well-known guest-side bind-mount path for the
// persistent, shared IPC namespace created on demand via the guest's
// PodNamespaces.EnsureNamespaces TTRPC call (see
// internal/vminit/podns.Manager).
const IPCPath = "/run/ipcns/pod"

// PIDPath is the well-known guest-side bind-mount path for the
// persistent, shared PID namespace, anchored by a pod-pause process (see
// internal/vminit/podpause) created on demand via the same call.
const PIDPath = "/run/pidns/pod"
