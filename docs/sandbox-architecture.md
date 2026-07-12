# Sandbox Architecture

This document describes how the nerdbox sandbox works — what lives on the
host, what lives in the VM, and how networking flows between them.

## Overview

A nerdbox **sandbox** is a single microVM that hosts one or more containers.
It maps directly to the Kubernetes pod model: one VM per pod, with all
containers in the pod sharing the VM's kernel, network stack, and IPC
facilities.

```
┌─────────────────────────────────────────────────────────────────────┐
│  Host (Linux)                                                       │
│                                                                     │
│  ┌────────────────────────────────────┐                            │
│  │  containerd                        │                            │
│  │  ┌──────────────────────────────┐  │                            │
│  │  │  Sandbox Controller (shim)   │  │                            │
│  │  │  • CreateSandbox             │  │                            │
│  │  │  • StartSandbox              │  │                            │
│  │  │  • Task.Create (per ctr)     │  │                            │
│  │  └──────────────┬───────────────┘  │                            │
│  └─────────────────┼──────────────────┘                            │
│                    │ TTRPC (vsock 1025)                             │
│                    │                                                │
│  ┌─────────────────▼──────────────────────────────────────────┐    │
│  │  VMM (libkrun)                                             │    │
│  │                                                            │    │
│  │  ┌─────────────────────────────────────────────────────┐  │    │
│  │  │  vminitd (PID 1)                                    │  │    │
│  │  │                                                     │  │    │
│  │  │  ctr-A (runc)   ctr-B (runc)   ctr-C (runc)        │  │    │
│  │  │  ┌──────┐       ┌──────┐       ┌──────┐            │  │    │
│  │  │  │  /   │       │  /   │       │  /   │            │  │    │
│  │  │  └──────┘       └──────┘       └──────┘            │  │    │
│  │  │  shared: network, IPC, /dev/shm (kernel)            │  │    │
│  │  └─────────────────────────────────────────────────────┘  │    │
│  └────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
```

The runtime referenced above as "runc" is the OCI runtime interface. nerdbox
uses crun as its implementation, but the interface and container model follow
the runc specification.

## Host / VM responsibility split

Everything that needs to interact with the host OS — CNI plugins, image
snapshotters, volume mounts — is managed on the **host side** of the shim.
Everything that needs to interact with a running container process —
namespace setup, cgroup accounting, syscall filtering — is managed
**inside the VM** by vminitd.

### What the host shim owns

| Resource | Where it lives | Notes |
|---|---|---|
| VM lifecycle | Host shim process | libkrun starts/stops the VM; the shim holds the only reference |
| Container rootfs assembly | Host filesystem | Overlay / erofs layers mounted on host, exposed to VM via virtiofs |
| Bind mounts and volumes | Host filesystem | Resolved and mounted on the host inside the shim's mount namespace, exposed via the same virtiofs share |
| Network sandbox (netns path) | Host shim process | FD held open for the CNI lifetime — see [Networking](#networking) |
| Virtual NICs | Host VMM config | Configured before VM boot via libkrun; cannot be added after boot |
| Socket forwarding | Host shim | UNIX sockets forwarded host↔VM via the SocketForward TTRPC service |
| OCI bundle (config.json) | Host shim, pushed to guest | Assembled on host from snapshotter metadata, pushed to guest over the Bundle TTRPC service |

### What the guest (vminitd) owns

| Resource | Where it lives | Notes |
|---|---|---|
| Container process lifecycle | VM | runc creates/starts/stops containers |
| Mount namespaces | VM kernel | Each container gets its own mount namespace; rootfs is bind-mounted from the virtiofs share |
| cgroups (v2 unified) | VM kernel | One cgroup per container, under vminitd's cgroup tree |
| Network namespaces | VM kernel | All containers share the VM init namespace by default; per-container network isolation is supported via OCI spec |
| IPC / /dev/shm | VM kernel | Shared IPC namespace, created on demand, when CRI's pod-level IPC sharing is requested (see [Pod PID and IPC namespace sharing](#pod-pid-and-ipc-namespace-sharing)); otherwise each container gets its own |
| PID namespace | VM kernel | Own PID namespace by default; joins a shared, on-demand pod PID namespace when CRI's pod-level PID sharing is requested (see [Pod PID and IPC namespace sharing](#pod-pid-and-ipc-namespace-sharing)) |
| Hostname / UTS | VM kernel | Inherited from the VM init namespace unless overridden by the container OCI spec |

## Container filesystem

Each container's rootfs is assembled **on the host** inside the shim's
private mount namespace, then shared into the VM via a single persistent
virtiofs mount.

```
Host state directory:  <shim-bundle>/vm/
Virtiofs share root:   <shim-bundle>/vm/containers/   ← tag "containers"
Guest mount point:     /run/containers/

Per-container tree:
  /run/containers/<container-id>/rootfs    ← assembled from snapshotter mounts
  /run/containers/<container-id>/volumes/0 ← first extra volume (if any)
```

The host-side assembly **mounts the rootfs at the correct path or fails**.
The mount type is determined by the snapshotter and containerd:

- **Overlay mount** — overlayfs over multiple layer directories (the common
  case with the native overlayfs snapshotter).
- **Bind mount** — a single pre-extracted directory bind-mounted read-only
  (used by native snapshotter with a fully-extracted layer, or nydus).
- **FUSE mount** — a FUSE-based filesystem exposed by an external snapshotter
  (e.g. stargz-snapshotter, nydus).

If none of these mounts can be established, the container run fails. There is
no fallback to hard links or file copies — both would produce silent failures:
hard links can fail across filesystems, and copies accumulate dirty pages and
destroy filesystem metadata.

After `Task.Delete`, `SharedFS.Unshare` removes the container's subtree from
the shared directory, unmounting any mounts and calling `os.RemoveAll` on the
directory entry.

```
┌── Host shim → vmm ────────────────────────────────────────────────┐
│                                                                    │
│  snapshotter mounts                                                │
│  ┌──────────────────┐                                             │
│  │ erofs layer A    │                                             │
│  │ erofs layer B    │ ──── mount (overlay/bind/fuse) ───►         │
│  │ ext4 upper       │                                  │          │
│  └──────────────────┘                                  ▼          │
│                                     vm/containers/<id>/rootfs     │
│                                             │                     │
└─────────────────────────────────────────────┼─────────────────────┘
                                              │ virtiofs (tag "containers")
                                              ▼
                                 vminitd: /run/containers/<id>/rootfs
                                              │
                                              │ bind mount (by runc)
                                              ▼
                                 Container rootfs in its mount namespace
```

## Networking

Networking involves two independent layers that are often confused:

1. **The host-side network sandbox** — a Linux network namespace on the host,
   created and owned by the CRI layer (containerd), passed to the shim.
2. **The VM-side network stack** — the actual network interfaces the containers
   use, configured inside the microVM.

### Layer 1 — Host network sandbox (Linux netns)

#### How the netns is created

The CRI layer (containerd's CRI plugin, running in the containerd process)
creates the network namespace entirely by itself — no pause container is
involved. The mechanism is the long-standing CNI "persistent netns" technique:

1. A dedicated goroutine calls `runtime.LockOSThread()` and never unlocks,
   so Go retires the underlying OS thread when the goroutine exits (Go 1.10+).
2. On that locked thread, `unshare(CLONE_NEWNET)` creates a new, empty
   network namespace for that thread only
   (`pkg/netns/netns_linux.go:116` in containerd).
3. The thread's netns is bind-mounted to a file under `/var/run/netns/`
   (or the configured state dir) via `mount("/proc/<containerd-pid>/task/<tid>/ns/net",
   "/var/run/netns/cni-<random>", MS_BIND)`.
   Here `<containerd-pid>` is the containerd process PID and `<tid>` is the
   TID of the dedicated throwaway thread — `/proc/self/ns/net` cannot be used
   because it always returns the thread-group-leader's namespace.
4. The bind-mount anchors the netns to the filesystem. The throwaway thread
   exits but the namespace persists because the bind-mount still holds a
   reference. **A netns persists with zero processes in it as long as the
   bind-mount file exists.**

#### Ordering: CNI runs before the sandbox

```
containerd CRI plugin (RunPodSandbox)

  1. Create netns bind-mount at /var/run/netns/cni-<id>    ← unshare + bind
  2. Run CNI ADD against that empty netns                   ← configures IP/routes/etc
  3. CreateSandbox(netns_path=/var/run/netns/cni-<id>)     ← shim receives path
  4. StartSandbox                                           ← shim boots VM
```

CNI **always runs before the sandbox is created**. CNI configures an empty,
process-less netns (which it can do because the bind-mount keeps it alive),
and the sandbox is later started knowing the fully-configured path.

With the **shim sandboxer there is no pause container** — the shim receives
`netns_path` directly in `CreateSandboxRequest`. (The legacy `podsandbox`
controller creates a pause container which *joins* the pre-existing netns via
an OCI `LinuxNamespace{Type: network, Path: nsPath}`; the shim sandboxer skips
this entirely.)

For host-network pods (`NamespaceMode_NODE`), no netns is created and
`netns_path` is empty.

#### What the shim does with netns_path

**At `CreateSandbox` time** the shim opens the path `O_RDONLY|O_CLOEXEC` and
holds the FD open. This second reference to the netns (alongside the
bind-mount) keeps it alive even if the bind-mount were removed prematurely,
and satisfies the CRI contract. The shim releases this FD after `StopSandbox`.

```
CRI layer                        nerdbox shim
    │                                │
    │── CreateSandbox(netns_path) ──►│  opens FD to netns_path
    │                                │  (secondary pin on the bind-mount)
    │── StartSandbox ───────────────►│  libkrun FFI thread enters netns
    │                                │  VM boots
    │                                │  FD remains open
    │   [ pod running ]              │
    │                                │
    │── StopSandbox ────────────────►│  VM stops
    │                                │  FD closed
    │   [ CNI DEL runs against netns_path ]
    │── ShutdownSandbox ────────────►│  final cleanup
```

**At `StartSandbox` time** the netns path is passed to the libkrun FFI
executor thread (see [Layer 2](#layer-2--vm-network-stack) below), which
enters the pod netns before any libkrun calls open host resources.

### Layer 2 — VM network stack

#### The libkrun FFI executor

All libkrun FFI calls for a VM context (`krun_create_ctx` through
`krun_start_enter`) run on a **single dedicated OS thread** (the "executor
thread") that holds `runtime.LockOSThread()` for its entire lifetime. This is
necessary because:

- libkrun opens host-side resources (NIC AF_UNIX sockets, TSI host sockets)
  on the calling thread.
- libkrun's internal worker threads (vCPU, virtio backends, TSI net workers)
  are spawned as children of the thread that calls `krun_start_enter` and
  inherit its network namespace.
- Go's scheduler may migrate goroutines across OS threads; without pinning,
  each `krun_*` call could run in a different namespace.

When `netns_path` is non-empty, the executor thread calls `setns(2)` into the
pod netns **before** `krun_create_ctx`. Every subsequent libkrun call and
every thread libkrun spawns thereafter is automatically inside the pod netns.

```
localsandbox.Start()
    │
    ├── vmm.NewInstance()
    │       └── [executor goroutine: LockOSThread, stays alive]
    │
    ├── vmi.SetNetnsPath(netns_path)        ← setns on executor thread
    │
    ├── vmi.AddDisk(...)                    ← all on executor thread
    ├── vmi.AddFS(...)                      ← all on executor thread
    ├── vmi.AddNIC(...)                     ← NIC AF_UNIX socket opened in pod netns
    ├── vmi.SetCPUAndMemory(...)
    │
    └── vmi.Start()
            └── krun_start_enter            ← blocks on executor thread
                    │
                    ├── vCPU thread         ← inherits pod netns
                    ├── virtio workers      ← inherits pod netns
                    └── TSI net workers     ← inherits pod netns
                            │
                            └── host connect(AF_INET, ...) ← in pod netns
```

Control-plane goroutines (the shim TTRPC listener, vsock accept, vminitd
connection) operate over FD-based UDS/vsock connections established before
`setns` and are unaffected by the namespace change.

**Validated end-to-end:** `ContainerTrafficScopedToNetworkSandbox`
(shimtest, root-gated) passes, confirming the executor's in-process
`setns` is sufficient — a member container's outbound traffic actually
originates from the pinned pod netns, not the shim's own. (Getting this
test to run as real root required two unrelated fixes: `cloneMntNs`
was unconditionally demoting the shim into a *new* user namespace even
when already real root, which broke real block-device mounts; and
`SharedFS.ShareRootfs` was calling the generic containerd `mount.All`
instead of nerdbox's own `mountutil.All`, which is what understands the
`X-containerd.mkdir.*` options used to build overlay upper/work dirs. Both
fixed in `pkg/shim/manager/mount_linux.go` and
`internal/shim/sandbox/sharedfs.go`.) No re-exec/trampoline pivot was
needed.

#### TSI (Transparent Socket Impersonation)

TSI is **not configured by the shim** — it is a compiled-in feature of the
guest kernel (`CONFIG_TSI=y`, patches `0009`–`0012` in `kernel/patches/`).

Inside the VM, the patched kernel intercepts `AF_INET` socket calls
(TCP/UDP). When a container opens a TCP connection, the kernel transparently
rewrites it to `AF_TSI` and proxies it over vsock to libkrun, which performs
the real `connect()` on the host — now inside the pod netns (after the
executor thread's `setns`).

```
Container (guest)                    Host (pod netns)
                                     ┌──────────────────────┐
 connect(AF_INET, 1.2.3.4:80)        │  libkrun TSI worker  │
       │                             │  (executor thread     │
  TSI kernel intercept               │   lineage, pod netns) │
       │                             │                       │
       │ ── vsock ──────────────────►│  connect(1.2.3.4:80)  │
                                     │  source: pod IP        │
                                     └──────────────────────┘
```

TSI limitations: IPv4 TCP/UDP only. ICMP, raw sockets, and IPv6 are not
supported.

##### Fixed: TSIv2/TSIv3 wire-protocol mismatch

Conformance testing (`NetworkSuite` and `ContainerOutboundTCP` in shimtest)
initially found that TSI did not establish outbound connections at all — a
container's `connect()` never completed, and `strace` on the host process
showed the host-side `connect()`/`socket()` syscall was never even reached.

Root cause: the kernel patches in `kernel/patches/` implemented an **older
TSI wire protocol (TSIv2)** — `tsi_connect_req { u32 svm_port; u32 addr;
u16 port; }`, a bare IPv4 address — while the bundled libkrun (v1.19.0)
implements **TSIv3**, which uses a length-prefixed, family-tagged address
(`{ u32 svm_port; u32 addr_len; char addr[128]; }`) to support IPv6/AF_UNIX.
libkrun's TSIv3 parser silently misinterpreted the guest's TSIv2 payload
(reading the raw IPv4 address as a bogus `addr_len`), so every connect
request was dropped before any host socket call was made. This was never
caught previously because no test in this repository (or CI) exercised TSI
end-to-end before this pass.

**Fix:** the kernel patches were replaced with upstream libkrunfw's current
TSIv3 patches (`0011`/`0012`, plus two previously-missing vsock prerequisites,
`0009`/`0010`), matching the wire protocol libkrun v1.19.0 expects. Verified:
all patches apply cleanly (`patch -p1 --fuzz=0`) against a real 6.12.46
kernel tree; `ContainerOutboundTCP` and `NetworkSuite/{OutboundTCP,
OutboundUDP,DNSResolve}` all pass against the rebuilt kernel. The Dockerfile
patch-apply loop was also hardened with `set -e` (previously a failed hunk
would silently continue, producing an unpatched kernel with no build error).

##### Known limitation: connected UDP sockets to loopback destinations

TSI's `tsi_connect()` tries the guest's own local `AF_INET` socket first;
only if that local `connect()` fails does it fall back to proxying via
vsock to the host. For **UDP**, a local `connect()` is a purely local
kernel operation — it succeeds immediately whenever the routing table has
*any* route to the destination, with no live handshake. In the default
no-NIC guest (only `lo` configured), that is true for **loopback**
destinations (`127.0.0.0/8`, always locally routable) but false for real
external IPs (no default route without a NIC, so `connect()` fails with
`ENETUNREACH` and correctly falls through to the vsock/host proxy).

Net effect: an application using a "dial once, then read/write" UDP pattern
(a *connected* UDP socket, e.g. `net.Dial("udp", ...)` in Go) against a
**loopback** destination gets silently locked to the guest's own isolated
network stack and never reaches the host — even though the exact same
pattern against a real external IP works correctly. Per-datagram
"unconnected" UDP (`sendto`/`recvfrom`, e.g. `net.ListenPacket` +
`WriteTo`/`ReadFrom` in Go) is unaffected: TSI checks for a local listener
on every message and proxies to the host when there isn't one.

This surfaced in practice as a DNS resolution failure: Go's standard
resolver uses connected UDP internally, and many Linux distributions
(anything using systemd-resolved) point `/etc/resolv.conf` at a loopback
stub resolver (`127.0.0.53`). Copying that file verbatim into the guest (the
`addResolvConf` fallback path) produced a `resolv.conf` whose nameserver is
unreachable from inside the VM.

This is not a nerdbox- or TSI-specific bug so much as a general
consequence of copying host DNS configuration into an isolated network
environment — Docker and containerd's CRI implementation handle the exact
same systemd-resolved case by preferring systemd-resolved's "full"
resolv.conf (`/run/systemd/resolve/resolv.conf`, which lists the real,
non-loopback upstream nameservers) over the stub file. `addResolvConf`
(`internal/shim/task/ctrnetworking.go`) now does the same: it detects an
all-loopback nameserver list and substitutes the full file when present.
No kernel change was needed or attempted for this — the underlying
connected-UDP-to-loopback behavior in TSI is left as-is (fixing it would
mean patching `tsi_connect()` to add dgram-aware, loopback-aware fallback
logic in `af_tsi.c`, diverging further from upstream; there is no known
open upstream issue for this specific case, likely because most libkrun
consumers do not blindly copy the host's raw `resolv.conf`).

##### Known limitation: TSI ignores guest-internal network namespaces

TSI provides no network-namespace isolation *inside the guest*. The kernel
patch's socket hijack (`__sock_create` rewriting `AF_INET`/`AF_INET6` to
`AF_TSI`/`AF_TSI6`) triggers purely on address family, before any
namespace-aware routing decision would occur, and the resulting vsock
channel to `VMADDR_CID_HOST` is not real IP routing — it is not subject to
netns scoping, and (since no real `AF_INET` socket ever exists) it cannot
be filtered by guest-side `iptables`/`nftables` either.

Concretely: placing a container in its own, brand-new guest network
namespace (an explicit, empty-`Path` `NetworkNamespace` entry in the OCI
spec — real `crun`-level netns isolation, not the host-side sandbox netns
pinning described above) does **not** stop it from reaching a host TCP
listener via TSI. Verified empirically: a container so configured
successfully completed a full TCP round trip to a host listener bound to
`127.0.0.1`.

**The practical model:** when TSI is enabled (the default), treat the
*entire guest kernel* as a single network namespace with respect to host
reachability — guest-internal network namespaces (per-container or
otherwise) provide **container-to-container** isolation (via the normal
veth/bridge mechanisms in `internal/vminit/ctrnetworking`) but provide
**no host-isolation boundary**. The only real host-isolation boundary is
the host-side one described in [Layer 1](#layer-1--host-network-sandbox-linux-netns)
above: the pod netns the shim pins and the executor thread `setns`s into,
which determines *which host network* TSI's proxied connections land in.
A container cannot escape that host-side scoping by manipulating its own
guest netns — but by the same token, no guest-side netns configuration
narrows it either. If per-container host-isolation stronger than the pod's
own netns is ever required, TSI would need to become namespace-aware in
the kernel (e.g. scoping the hijack or the vsock proxy per calling netns);
that has not been implemented and is being deliberately deferred rather
than treated as a bug to fix silently, since it changes TSI's contract.

##### Known limitation: TSI does not mirror the host's socket table

The flip side of the above: TSI provides *outbound connection* reachability
by proxying individual `connect()`/`listen()` calls over vsock — it does
not give the guest any *introspectable* view of the host's own network
stack. A container cannot, for example, run `netstat`/`ss` and see the
host's own listening sockets, the way a process would under a real Linux
"host network" mode (`hostNetwork: true` in Kubernetes) where the
container genuinely shares the host's network namespace and its socket
table is the host's socket table.

This means CRI's `HostNetwork: true` conformance check (`critest`'s
"runtime should support HostNetwork is true", which starts a listener on
the host and expects `netstat -ln` run inside the container to show it)
cannot be satisfied by TSI, or by anything this shim does with guest
network namespaces — see test/critest/README.md's "Known conformance
gaps". Providing genuine host-socket-table visibility would require a
fundamentally different networking mode from TSI (e.g. real host network
namespace passthrough into the guest), which is not implemented and is a
much larger change than a namespace-sharing fix.

#### External NIC (explicit virtio-net)

When the OCI spec annotations carry `io.containerd.nerdbox.network.*`, a
virtio-net NIC is attached to the VM. The NIC is backed by an AF_UNIX socket
(`krun_add_net_unixgram` or `krun_add_net_unixstream`) that connects libkrun
to an **externally-run** L2 network provider.

This AF_UNIX socket is opened on the executor thread (already in the pod
netns), so the connection to the external provider originates from the pod
netns.

Supported external providers:
- **passt** (unixgram mode) — passt-style helpers that exchange complete L2
  Ethernet frames as datagrams.
- **gvproxy / vfkit** (unixstream mode) — helpers that frame L2 packets over
  a stream connection.

The shim does **not** spawn the external provider. The user (or a future
shim enhancement) must run it out-of-band and pass its socket path via
annotation. Note: `krun_set_gvproxy_path` and `krun_set_net_mac` are declared
in the libkrun bindings but are currently unused.

```
External network provider          nerdbox shim (pod netns)
(passt / gvproxy)                      │
         │                             │
         │ AF_UNIX socket (L2 frames)  │
         └────────────────────────────►│ libkrun: AddNIC(socket)
                                       │
                                       ▼
                            VM: virtio-net interface (eth0)
                            vminitd brings up eth0 with IP/routes
                                       │
                              ┌────────┴──────────┐
                              │                   │
                         Container A          Container B
                         (veth in its         (shared eth0 or
                          own netns)           own veth pair)
```

The NIC is configured before VM boot and cannot be changed while the VM runs
(libkrun does not support device hotplug).

### What socketforward is not

The socketforward service (vsock port 1026) forwards **AF_UNIX domain sockets**
host↔guest over vsock streams. It is not IP networking: both ends are
`net.Listen("unix", ...)` / `net.Dial("unix", ...)`. AF_INET/TCP
networking is handled exclusively by TSI (default) or the virtio-net NIC
(opt-in). These three mechanisms are independent and must not be conflated.

### Sandbox networking summary

| Scenario | Host netns | VM network |
|---|---|---|
| No annotation (default) | Pinned (FD + entered by executor thread) | TSI — AF_INET TCP/UDP through pod netns |
| `io.containerd.nerdbox.network.*` | Pinned (FD + entered by executor thread) | virtio-net NIC; AF_UNIX to external provider from pod netns |
| Kubernetes CRI pod | Created by containerd CRI (`unshare` + bind-mount); CNI ADD before sandbox | Either of the above, with full pod netns integration |
| `ctr run` (no sandbox) | No netns (legacy single-container path) | TSI or virtio in shim's own netns |
| Host-network pod (`NamespaceMode_NODE`) | Not created; `netns_path` is empty | TSI or virtio in shim's own netns |

## Pod PID and IPC namespace sharing

Kubernetes pods share an IPC namespace by default, and can opt into sharing
a PID namespace (`shareProcessNamespace: true`) or the node's PID/IPC
namespaces (`hostPID`/`hostIPC: true`). containerd's `WithPodNamespaces`
oci-spec opt expresses all of these the same way: it sets a host path (e.g.
`/proc/<sandboxPid>/ns/ipc`) on the relevant namespace entry of a member
container's OCI spec. That host path is meaningless in the guest — the
guest is a different kernel with its own, unrelated PID/IPC namespaces —
so, exactly as with the network namespace (see
[TSI ignores guest-internal network namespaces](#known-limitation-tsi-ignores-guest-internal-network-namespaces)
above), the shim must recognize the request and substitute a guest-side
equivalent rather than copying the host path verbatim.

### Mechanism

Unlike the network namespace (created unconditionally at vminitd startup —
see `internal/podnetns`), the shared PID and IPC namespaces are created
**on demand**, the first time any member container's spec actually asks
for one of them, via a small guest-side TTRPC service
(`internal/vminit/podns`, registered as plugin `podns`):

- **IPC**: created the same way as the shared network namespace — a
  dedicated goroutine locks itself to an OS thread, calls
  `unshare(CLONE_NEWIPC)` (which, unlike `CLONE_NEWPID`, takes effect on
  the calling thread immediately), and bind-mounts
  `/proc/self/task/<tid>/ns/ipc` to a well-known path
  (`/run/ipcns/pod`). The bind mount alone keeps the namespace alive.
- **PID**: a PID namespace has no content of its own and is torn down
  (every process in it killed) the instant its PID 1 exits, so it cannot
  be anchored by a bind-mount alone the way IPC and network namespaces
  can. `unshare(CLONE_NEWPID)` also does not move the calling
  thread/process into the new namespace — it only causes the *next
  forked child* to become PID 1 of a new namespace. So the guest instead
  execs a real, persistent anchor process (vminitd re-execs itself with a
  hidden `pod-pause` argument — see `internal/vminit/podpause`) with
  `SysProcAttr.Cloneflags: CLONE_NEWPID`, then bind-mounts
  `/proc/<anchor-pid>/ns/pid` to `/run/pidns/pod`. The anchor ignores
  every signal except SIGKILL and reaps any process reparented to it (a
  PID-1-of-namespace duty), and is only ever killed by the host at
  sandbox teardown.

On the host side, `internal/shim/task/podnetns.go`'s `sanitizeNamespaces`
bundle transformer (which already rewrites the network namespace path)
also handles IPC and PID: any IPC or PID namespace entry with a non-empty
incoming `Path` is treated as "share within this pod" and rewritten to
point at the guest's shared namespace, fetched lazily (and memoized per
`Task.Create` call) via `internal/shim/task/podns.go`'s `sharedNamespaces`
— a TTRPC client wrapper around the guest's `PodNamespaces.EnsureNamespaces`
call. A container whose spec has no such entry at all (the common case: no
pod-level sharing requested) never triggers the guest RPC, and therefore
never causes the guest to spawn the pod-pause anchor process, at all.

### HostPID / HostIPC vs. PodPID: an unavoidable simplification

containerd sets the *same* host path (derived from the sandbox's own PID)
for both `NamespaceMode_POD` (pod-level sharing) and `NamespaceMode_NODE`
(`hostPID`/`hostIPC: true`) — there is no data in the request that lets the
shim tell them apart. This shim deliberately does not try: any non-empty
incoming `Path` is treated identically, redirected to the pod's shared
guest namespace. In practice this is sufficient for real CRI conformance
(see test/critest/README.md) for everything except a `hostIPC: true` test
that plants a SysV shared memory segment directly on the **real host
machine** before creating the sandbox — no VM-internal namespace can make
guest processes see an object that only exists in a different kernel
entirely. `HostPID`, `HostIpc is false`, and `PodPID` all pass, because
they only depend on cross-container visibility *within the same pod*,
which the shared guest namespace genuinely provides regardless of which
CRI namespace mode nominally asked for it.

## Sandbox lifecycle

```
containerd                      nerdbox shim                  VM
    │                                │
    │── CreateSandbox ──────────────►│  alloc state dir
    │   (netns_path)                 │  create shared fs root
    │                                │  open netns FD (pin)
    │
    │── StartSandbox ───────────────►│  parse bundle for resources/NICs
    │                                │  start executor thread (LockOSThread)
    │                                │  setns into pod netns (if set)
    │                                │  add virtiofs "containers" share
    │                                │  start VM ────────────────────►│ boot
    │                                │                                 │ vminitd starts
    │                                │◄── TTRPC connect (vsock 1025) ──│
    │
    │── Task.Create (ctr-A) ────────►│  ShareRootfs: mount rootfs
    │                                │    on host in shared dir
    │                                │  Bundle.Create ───────────────►│
    │                                │  Mount.MountAll ──────────────►│ bind rootfs
    │                                │  Task.Create ─────────────────►│ runc create
    │
    │── Task.Start (ctr-A) ─────────►│  Task.Start ──────────────────►│ runc start
    │                                │                                 │ container runs
    │
    │── Task.Create (ctr-B) ────────►│  (same flow, same VM)
    │── Task.Start (ctr-B) ─────────►│
    │
    │   [ pod running ]
    │
    │── Task.Delete (ctr-A) ────────►│  Task.Delete ─────────────────►│ runc delete
    │                                │  SharedFS.Unshare(ctr-A)        │
    │                                │    unmount rootfs on host        │
    │                                │    remove shared dir entry       │
    │
    │── StopSandbox ───────────────►│  SharedFS.UnshareAll
    │                               │  VM.Stop ──────────────────────►│ shutdown
    │                               │  netns FD closed (unpin)
    │
    │   [ CNI DEL runs on host ]
    │
    │── ShutdownSandbox ───────────►│  (idempotent stop if needed)
```

## TTRPC communication

The host shim and vminitd communicate over two vsock channels:

```
Host shim                           vminitd (guest)
    │                                   │
    │◄── vsock port 1025 (TTRPC) ──────►│
    │    Task, Bundle, Mount,            │
    │    System, SocketForward,          │
    │    Events services                 │
    │                                   │
    │◄── vsock port 1026 (streams) ────►│
    │    stdio (stdout/stderr/stdin)     │
    │    transfer service data           │
```

vminitd **dials back** to the host on port 1025 (not the other way around),
which allows the host to accept the connection without needing to know the
guest CID in advance.

## Security properties

- The shim process runs in its own **mount namespace** (`CLONE_NEWNS`), plus a
  **new user namespace** (`CLONE_NEWUSER`) when it is not already real root —
  unprivileged callers gain CAP_SYS_ADMIN within that namespace to perform
  rootfs mounts. When the shim is already real root (e.g. under `sudo`),
  `CLONE_NEWUSER` is deliberately skipped: entering a *new* user namespace,
  even one mapping root to root, demotes the process to a non-initial user
  namespace, and the kernel restricts mounting real block-device-backed
  filesystems (ext4, used for the sandbox scratch/overlay mounts) to the
  initial user namespace regardless of capabilities held within a descendant
  one. Either way, mounts created for container rootfs assembly are isolated
  from the host and cleaned up automatically when the shim exits.
- Container processes run inside the VM guest kernel. The guest kernel is a
  different kernel instance from the host, providing strong isolation.
- The virtiofs share is writable (host-to-guest) but each container's subtree
  is isolated: one container cannot see or modify another container's files
  within the shared tree.
- The network sandbox FD is opened `O_RDONLY | O_CLOEXEC`. The FD is used
  only to pin the bind-mount and (via `SetNetnsPath`) to enter the pod netns
  on the executor thread. The shim's control-plane goroutines remain in the
  shim's original network namespace.

## Future work

The following capabilities are planned but not yet implemented:

- **Turnkey virtio networking** — have the shim spawn and manage a passt or
  gvproxy process (inside the pod netns) rather than requiring a user-supplied
  socket path via annotation.
- **Shared `/dev/shm`** — a per-sandbox tmpfs shared across all containers in
  the VM, matching the Kubernetes pod `shm` mount contract.
- **Shared volumes (emptyDir)** — a cross-container shared directory exposed
  to multiple member containers.
- **Single ext4 upper layer** — a forthcoming containerd change will support
  placing multiple container upper filesystems in one ext4 image, which can be
  mounted upfront and eliminate per-container mount overhead on non-root hosts.
