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
│  │  │  shared, on demand: network, IPC, PID, /dev/shm    │  │    │
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
| Network namespaces | VM kernel | Containers share the VM init namespace when the sandbox has no NIC (nothing for a namespace to scope); a shared per-sandbox namespace is created on demand when it does (see [Sandbox networking summary](#sandbox-networking-summary)); per-container isolation is supported via OCI spec |
| IPC namespace | VM kernel | Shared, created on demand, when CRI's pod-level IPC sharing is requested (see [Shared guest namespaces](#shared-guest-namespaces)); otherwise each container gets its own |
| PID namespace | VM kernel | Own PID namespace by default; joins a shared, on-demand PID namespace when CRI's pod-level PID sharing is requested (see [Shared guest namespaces](#shared-guest-namespaces)) |
| /dev/shm | VM kernel | A container sharing IPC bind-mounts a shared, size-limited guest tmpfs; otherwise its own private one — see [Shared `/dev/shm`](#shared-devshm) |
| UTS namespace | VM kernel | Always its own, fresh namespace — there is no sharing mechanism for this type, unlike network/IPC/PID |
| Hostname | Host shim, pushed to guest | Set from the pod's CRI config on every member container's own UTS namespace — enough to give them all the same observable hostname without actually sharing the namespace |

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

The host-side assembly mounts the rootfs at the correct path, or fails the
container run if a required mount cannot be established. (An empty mount
list — legal for a from-scratch container with no image content — is not an
error: it simply leaves an empty directory in place.) The mount type is
determined by the snapshotter and containerd:

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

### Bind mounts and volumes

A member container's OCI "bind" mounts (Kubernetes `hostPath` volumes, and
CRI's own injected UDS/sandbox-file mounts) are handled by
`SharedFS.ShareVolume` rather than becoming a new virtiofs share: a sandbox
member container is created against an already-running VM, and virtio-fs
shares cannot be hot-added after boot, so each mount's host source is
instead bind-mounted directly into the container's own subtree of the
already-shared `containers` tree (`<container-id>/volumes/<n>`), which the
guest already sees with no new device and no extra guest-side mount step.

This mechanism is oblivious to whether a given host source is exclusive to
one container or handed to several: each container that references the same
host path simply gets its own independent bind mount of that path. This is
what makes Kubernetes `emptyDir` volumes work transparently across multiple
containers in one pod — kubelet provisions a single host directory per
`emptyDir` volume and lists that same host path in every container's mount
spec that references it, so all of them end up bind-mounting identical
content, with no sandbox-specific "shared volume" logic required.

### Shared `/dev/shm`

CRI sends `/dev/shm` as an ordinary, independent tmpfs mount
(`{Type: "tmpfs", Source: "shm", Options: [..., "size=<n>k"]}`) on **every**
container's spec — the mount itself carries no signal about whether the pod
actually wants it shared. That signal instead comes from the same place
namespace sharing does: `containerSharesIPC` (`internal/shim/task/devshm.go`)
checks for a non-empty-`Path` IPC namespace entry, the identical condition
[`sanitizeNamespaces`](#mechanism) uses to decide whether a container joins
the shared guest IPC namespace.

When that signal is present, `shareDevShmMounter.FromBundle` rewrites the
`/dev/shm` mount from `tmpfs` to a `bind` mount pointing at a per-sandbox
tmpfs created on demand in the **guest**, by the same `SharedResources`
mechanism [Shared guest namespaces](#shared-guest-namespaces) uses for
network/IPC/PID (`internal/vminit/sharedresources.TypeDevShm`) — a real,
`size=`-limited tmpfs mounted once per sandbox under `/run/devshm/<id>` in
vminitd's own root mount namespace, whose size comes from `devShmSize`
parsing the container's own CRI-provided `size=` option (64MiB fallback).
Every member container that shares IPC bind-mounts the *same* guest path
onto its own `/dev/shm`, so it is real, size-enforced guest RAM shared
across containers — not a host-backed directory standing in for one. When
the signal is absent, the mount is left untouched and each container keeps
its own independent, private tmpfs, matching CRI's non-shared default.

This tmpfs is mounted **outside** the `containers` virtiofs tree entirely,
in vminitd's own root mount namespace, rather than living inside the
directory `ShareRootfs`/`ShareVolume` expose via virtiofs: a member
container's own crun bind-mounts `/run/devshm/<id>` directly, the same way
it already joins `/run/netns/<id>` and `/run/ipcns/<id>` for shared
network/IPC namespaces, with no virtiofs involved at any point. This also
means it is real guest RAM with a real, kernel-enforced size limit, rather
than something backed by host disk and reached over virtiofs.
`mmap(MAP_SHARED)` writes from one container are visible to
`mmap(MAP_SHARED)` reads from another through ordinary Linux page-cache
coherence on the shared inode — since every sharing container's bind mount
ultimately resolves to the same tmpfs inode in the same guest kernel, this
holds regardless of virtiofs's own cache behavior.

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
   network namespace for that thread only (containerd's `pkg/netns`
   package).
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
and satisfies the CRI contract. The shim releases this FD as part of
handling `StopSandbox`, not afterward.

```
CRI layer                        nerdbox shim
    │                                │
    │── CreateSandbox(netns_path) ──►│  opens FD to netns_path
    │                                │  (secondary pin on the bind-mount)
    │── StartSandbox ───────────────►│  setns into netns, just before boot
    │                                │  VM boots
    │                                │  FD remains open
    │   [ pod running ]              │
    │                                │
    │── StopSandbox ────────────────►│  VM stops
    │                                │  FD closed
    │   [ CNI DEL runs against netns_path ]
    │── ShutdownSandbox ────────────►│  final cleanup
```

**At `StartSandbox` time** the netns path is stored on the `vmInstance` and
used just before boot (see [Layer 2](#layer-2--vm-network-stack) below):
`setns(2)` runs immediately before `krun_start_enter`, the last libkrun call
made on the boot path, so it takes effect right before libkrun opens any
host-side network resources.

### Layer 2 — VM network stack

#### Entering the pod netns before boot

Configuring a VM context (`krun_create_ctx` through the calls that add
disks, filesystems, NICs, and CPU/memory) does not itself open any
network-namespace-sensitive host resources, and can run on any goroutine —
Go's scheduler is free to migrate it across OS threads. Only the final step,
`krun_start_enter`, matters for namespace placement:

- `krun_start_enter` is what actually opens libkrun's host-side network
  resources (NIC AF_UNIX sockets, TSI host sockets) and spawns libkrun's
  internal worker threads (vCPU, virtio backends, TSI net workers) — those
  workers inherit the network namespace of the thread that called it.
- So the calling thread's network namespace at the moment of that one call
  is what determines which namespace all of libkrun's networking ends up in.

`vmInstance.Start` accounts for this by running `krun_start_enter` inside a
dedicated goroutine that calls `runtime.LockOSThread()` and, when a
`netns_path` was configured, calls `setns(2)` into the pod netns
**immediately before** `krun_start_enter` — as the last thing that happens
before boot, not the first:

```
vmInstance.Start()
    │
    └── goroutine: runtime.LockOSThread()
            │
            ├── setns(pod netns)            ← only if netns_path was set
            │
            └── krun_start_enter            ← blocks on this thread
                    │
                    ├── vCPU thread         ← inherits pod netns
                    ├── virtio workers      ← inherits pod netns
                    └── TSI net workers     ← inherits pod netns
                            │
                            └── host connect(AF_INET, ...) ← in pod netns
```

`AddNIC` itself only registers the NIC's socket path with libkrun (the FD
field is left at -1); libkrun opens the actual AF_UNIX socket to that path
itself, from inside `krun_start_enter`, so it too lands in the pod netns by
the same mechanism.

Control-plane goroutines (the shim TTRPC listener, vsock accept, vminitd
connection) operate over FD-based UDS/vsock connections established
independently of this goroutine and are unaffected by the namespace change.

The in-process `setns` is sufficient on its own: a member container's
outbound traffic originates from the pinned pod netns, with no re-exec or
trampoline process required.

#### TSI (Transparent Socket Impersonation)

TSI is a compiled-in feature of the guest kernel (`CONFIG_TSI=y`, patches
`0011`–`0012` in `kernel/patches/`; `0009`–`0010` are generic vsock support
patches, not TSI-specific), gated at runtime by the `tsi_hijack`
kernel parameter, which defaults to **off**. The shim does not set it: libkrun
does, and only when no virtio-net interface has been attached. TSI and a NIC
are alternative ways to provide the same connectivity, so libkrun enables TSI
exactly when there is no NIC (`enable_tsi = net.list.is_empty() && ...` in its
`VsockConfig::Implicit` handling). libkrun gates its own host side to match,
rejecting proxy requests when the hijack is disabled.

Inside the VM, the patched kernel intercepts `AF_INET` and `AF_INET6` socket
calls (`SOCK_STREAM`/`SOCK_DGRAM`, i.e. TCP/UDP). When a container opens a TCP connection, the kernel transparently
rewrites it to `AF_TSI` and proxies it over vsock to libkrun, which performs
the real `connect()` on the host — now inside the pod netns, since the
worker performing it descends from the `krun_start_enter` call made just
after `setns` (see [Layer 2](#layer-2--vm-network-stack)).

```
Container (guest)                    Host (pod netns)
                                     ┌──────────────────────┐
 connect(AF_INET, 1.2.3.4:80)        │  libkrun TSI worker  │
       │                             │  (descends from the   │
  TSI kernel intercept               │   krun_start_enter    │
       │                             │   thread, pod netns)  │
       │ ── vsock ──────────────────►│  connect(1.2.3.4:80)  │
                                     │  source: pod IP        │
                                     └──────────────────────┘
```

TSI covers TCP and UDP over both IPv4 (`AF_TSI`) and IPv6 (`AF_TSI6`). It does
not proxy ICMP or raw sockets, which stay in whatever network namespace the
container is in.

#### DNS configuration

Container resolv.conf content is resolved with the following priority: an
existing bundle mount, a per-container DNS annotation
(`io.containerd.nerdbox.ctr.dns`), the pod's CRI `DNSConfig`, and finally a
copy of the host's own resolv.conf. When falling back to the host's
resolv.conf, `addResolvConf` (`internal/shim/task/ctrnetworking.go`)
inspects the nameserver entries: only if **every** nameserver in
`/etc/resolv.conf` is a loopback address (the systemd-resolved stub
configuration, unreachable from inside the guest in the default
no-NIC/TSI configuration) does it substitute systemd-resolved's "full"
resolv.conf (`/run/systemd/resolve/resolv.conf`, listing the real upstream
nameservers) instead. A host not using systemd-resolved, or one with a mix
of loopback and real nameservers, is left as-is.

#### TSI and guest network namespaces

TSI's socket hijack operates on address family alone, before any
namespace-aware routing decision, and the resulting vsock channel to
`VMADDR_CID_HOST` is not real IP routing — so it is not scoped by, and
cannot be filtered via, guest-internal network namespaces. The
host-reachability boundary is established entirely on the host side: the pod
netns the shim pins and enters via `setns` just before boot (see
[Layer 1](#layer-1--host-network-sandbox-linux-netns) and
[Layer 2](#layer-2--vm-network-stack) above) determines which host network
TSI's proxied connections land in.

Because of this, the shim does not create a guest network namespace at all
when TSI is carrying container traffic: one would be created, joined, and then
ignored. Containers instead stay in the VM's own network namespace, but that
sharing is largely incidental to cross-container connectivity: TSI hijacks
each `socket()` call before any netns-scoped routing decision is ever made,
so two sibling containers reaching each other over loopback are not really
using guest-kernel loopback routing at all — each side's hijacked socket is
proxied independently to the host, and they only rendezvous because both
proxied operations resolve to the same concrete host-side port. This
requires the guest and the host to agree on that port: an inbound bind on an
ephemeral port (`bind()`/`listen()` on port `0`) must forward the guest
kernel's *resolved* port to the host, not the literal `0` the application
requested — otherwise the host independently picks its own, unrelated
ephemeral port, and nothing is reachable at the port the application
believes it bound (see kernel patch
`0013-tsi-forward-the-resolved-port-for-ephemeral-binds.patch`). This in
turn means the guest's resolved port can now collide with something
already bound on the host — see
[Known limitations](#known-limitations) for what happens then.

The decision needs nothing driver-specific. A network namespace exists to
scope in-guest networking, and a virtio-net interface is what creates that, so
the presence of a NIC decides it: no NIC means nothing to scope. Where
containers do have their own NICs, the veth/bridge mechanisms in
`internal/vminit/ctrnetworking` provide container-to-container isolation.

TSI proxies individual outbound `connect()`/`listen()` calls; it does not
mirror the host's own socket table into the guest, so introspection tools
like `netstat`/`ss` run inside a container only see the container's own
guest-side connections, not the host's.

#### External NIC (explicit virtio-net)

When the OCI spec annotations carry `io.containerd.nerdbox.network.*`, a
virtio-net NIC is attached to the VM. The NIC is backed by an AF_UNIX socket
(`krun_add_net_unixgram` or `krun_add_net_unixstream`) that connects libkrun
to an **externally-run** L2 network provider.

Like the TSI host sockets, this AF_UNIX socket is opened by libkrun from
inside `krun_start_enter` (already in the pod netns by then), so the
connection to the external provider originates from the pod netns.

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

The socketforward service is a TTRPC service reached over the same control
channel as everything else (vsock port 1025 — see
[TTRPC communication](#ttrpc-communication)); it forwards **AF_UNIX domain
sockets** host↔guest. Only the forwarded payload itself — the data read from
and written to the forwarded UNIX sockets — is what rides the separate
streaming channel on port 1026. It is not IP networking: both ends are
`net.Listen("unix", ...)` / `net.Dial("unix", ...)`. AF_INET/TCP
networking is handled exclusively by TSI (default) or the virtio-net NIC
(opt-in). These three mechanisms are independent and must not be conflated.

### Sandbox networking summary

| Scenario | Host netns | VM network | Guest netns |
|---|---|---|---|
| No annotation (default) | Pinned (FD); entered via `setns` just before boot | TSI — TCP/UDP through pod netns | None; containers share the VM's own |
| `io.containerd.nerdbox.network.*` | Pinned (FD); entered via `setns` just before boot | virtio-net NIC; AF_UNIX to external provider from pod netns | Shared per-sandbox namespace |
| Kubernetes CRI pod | Created by containerd CRI (`unshare` + bind-mount); CNI ADD before sandbox | Either of the above, with full pod netns integration | As above |
| `ctr run` (no sandbox) | No netns (legacy single-container path) | TSI or virtio in shim's own netns | n/a (legacy path) |
| Host-network pod (`NamespaceMode_NODE`) | Not created; `netns_path` is empty | TSI or virtio in shim's own netns | As above |

Guest network namespaces follow NIC presence alone, with no per-driver
behaviour: a namespace is created when the sandbox has an interface for it to
scope, and not otherwise. This holds regardless of how a driver provides
connectivity without a NIC, since anything that bypasses in-guest IP routing
(TSI being one example) is by definition not something a network namespace
can scope.

## Shared guest namespaces

Kubernetes pods share an IPC namespace by default, and can opt into sharing
a PID namespace (`shareProcessNamespace: true`) or the node's PID/IPC
namespaces (`hostPID`/`hostIPC: true`). containerd's `WithPodNamespaces`
oci-spec opt expresses all of these the same way: it sets a host path (e.g.
`/proc/<sandboxPid>/ns/ipc`) on the relevant namespace entry of a member
container's OCI spec. That host path is meaningless in the guest — the
guest is a different kernel with its own, unrelated namespaces — so the shim
recognizes the request and substitutes a guest-side equivalent rather than
copying the host path verbatim.

### Mechanism

Guest namespaces are created **on demand** by a guest-side TTRPC service,
`SharedResources` (`internal/vminit/sharedresources`, registered as plugin
`sharedresources`). Resources are addressed by a group id — the sandbox ID —
plus a type, and are created once per `(id, type)` and reused thereafter.
The guest returns the path each resource is pinned at, so the host never
hardcodes a guest path.

The same service also manages one resource that is not actually an OCI
namespace: the tmpfs backing a sandbox's shared `/dev/shm` (`TypeDevShm`;
see [Shared `/dev/shm`](#shared-devshm)). It is addressed, created, and
reused the same way — "create once per group id, return a guest path" is
the same problem either way — so `SharedResources` covers namespaces and
this kind of resource together rather than needing a separate service.

Crucially, a caller requests **only the types it needs**, because the cost is
not uniform:

- **Network** and **IPC**: created by locking a goroutine to an OS thread,
  calling `unshare(CLONE_NEWNET)` / `unshare(CLONE_NEWIPC)` (which, unlike
  `CLONE_NEWPID`, take effect on the calling thread immediately), and
  bind-mounting the thread's namespace file to `/run/netns/<id>` or
  `/run/ipcns/<id>`. The bind mount alone keeps the namespace alive, so the
  creating goroutine does not need to stay running. Cheap. For network
  specifically, the fresh namespace also has its loopback interface (`lo`)
  brought up before the bind-mount step — a new network namespace's `lo`
  starts administratively down, and without this step, container-to-container
  loopback traffic within the shared namespace would fail.
- **PID**: cannot work that way. `unshare(CLONE_NEWPID)` does not move the
  caller into the new namespace — only the caller's *next child* becomes its
  PID 1 — so a thread can never itself be PID 1, and
  `/proc/self/ns/pid_for_children` has no value to bind-mount until that
  first child exists. The kernel also destroys a PID namespace the instant
  its PID 1 exits, after which no further process can be created in it, so a
  bind mount cannot substitute for a live process the way it can for the
  other types. The guest therefore starts a real anchor process,
  `/sbin/nerdbox-pause` (a small `no_std` Rust binary, `crates/pause`), with
  `SysProcAttr.Cloneflags: CLONE_NEWPID`, and bind-mounts
  `/proc/<anchor-pid>/ns/pid` to `/run/pidns/<id>`. The anchor ignores
  SIGINT and SIGTERM (`SIG_IGN`) so it cannot be torn down by a stray signal
  delivered inside the shared namespace, and reaps reparented children via
  `SA_NOCLDWAIT` (a PID-1-of-namespace duty) rather than an explicit
  `wait()` loop.
- **DevShm**: like network/IPC, needs no anchor process — a plain
  `mount("tmpfs", ...)` at `/run/devshm/<id>` persists on its own for as
  long as anything references it, with no separate process or bind-mount
  step required to keep it alive. Cheap, and the same "only if actually
  requested" reasoning applies: a pod that never shares IPC never causes
  this tmpfs to be created either, since `shareDevShmMounter` only asks for
  it when `containerSharesIPC` is true.

Requesting only what is needed matters most for the PID namespace: Kubernetes
shares pod IPC by default but shares PID only when explicitly asked, so a
service that created both together would spawn an anchor process for
effectively every pod, whether or not anything used it.

Sharing a PID namespace only changes what a container's processes can *see*
via `/proc` — it does not change what the shim's `Kill`/`Pids` TTRPC
handlers can *target*. Those requests identify a process by container ID
and exec ID, never by raw PID, and are resolved against that specific
container's own process table (`Kill`) or by running `crun ps
<container-id>` scoped to that container's own cgroup (`Pids`). A signal
sent to one container therefore cannot land on a PID-namespace peer's
process, even though that peer's processes are visible to it.

On the host side, `internal/shim/task/namespaces.go`'s `sanitizeNamespaces`
bundle transformer determines which namespaces a container needs, fetches
them from the guest in a single `SharedResources.Create` call (memoized per
`Task.Create` via `sharedResources`, in `internal/shim/task/sharedresources.go`),
and rewrites the spec's namespace paths to the returned guest paths. Any IPC
or PID namespace entry with a non-empty incoming `Path` is treated as
"share within this sandbox". A container whose spec has no such entry at
all (the common case: no pod-level sharing requested) never triggers the
guest RPC for that type, and therefore never causes the guest to create the
namespace on its behalf.

### HostPID / HostIPC vs. PodPID

containerd sets the *same* host path (derived from the sandbox's own PID)
for both `NamespaceMode_POD` (pod-level sharing) and `NamespaceMode_NODE`
(`hostPID`/`hostIPC: true`) — there is no data in the request that lets the
shim tell them apart, so both are treated identically: any non-empty
incoming `Path` is redirected to the pod's shared guest namespace. This
gives every member container of a pod a consistent, shared PID/IPC view
regardless of which CRI namespace mode requested it.

## Sandbox lifecycle

```
containerd                      nerdbox shim                  VM
    │                                │
    │── CreateSandbox ──────────────►│  alloc state dir
    │   (netns_path)                 │  create shared fs root
    │                                │  open netns FD (pin)
    │
    │── StartSandbox ───────────────►│  add virtiofs "containers" share
    │                                │  parse bundle for resources/NICs
    │                                │  configure VM context (disks, FS, NICs)
    │                                │  [goroutine: LockOSThread,
    │                                │   setns into pod netns (if set),
    │                                │   krun_start_enter]
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
    │    Events, SharedResources,        │
    │    Transfer services               │
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
  one. Whenever `CLONE_NEWNS` is actually applied (both the real-root branch
  and a successful userns branch — not the apparmor-restricted fallback
  described below, which sets no clone flags at all and shares the host's
  mount namespace directly), the shim's new mount namespace is also remounted
  `MS_REC | MS_SLAVE` on `/` before use. Without this, a mount namespace
  created under a host root whose own `/` has `shared` propagation (the
  common default) leaks every mount the shim makes back out into the host's
  mount table; `MS_SLAVE` stops that leak in the host-visible direction while
  still letting the shim's namespace receive host-side mount/unmount events.
  Mounts made for container rootfs assembly are explicitly torn down by
  `SharedFS.Unshare`/`UnshareAll` on `Task.Delete`/`StopSandbox` — nothing
  relies on process exit to clean them up. This also holds when the shim
  exits *without* running that cleanup — e.g. `SIGKILL`, a panic, or an OOM
  kill — because `MS_SLAVE` means the host's mount table never had a copy of
  the shim's mounts in the first place; the kernel discards them
  unconditionally the moment the shim's mount namespace has no more
  references, regardless of how the shim exited. (Confirmed directly:
  killing a running shim with `SIGKILL` left zero entries for its bundle in
  the host's own mount table, and the kernel promptly reused the freed mount
  namespace's inode number for the next sandbox — evidence the namespace and
  everything in it was fully reclaimed, not merely orphaned.) The one case
  this doesn't cover is the apparmor-restricted fallback mentioned above: it
  runs the shim directly in the host's own mount namespace, so a crash there
  leaves real host mounts behind exactly as it would for any ordinary
  process — see [Known limitations](#known-limitations).
- Container processes run inside the VM guest kernel. The guest kernel is a
  different kernel instance from the host, providing strong isolation.
- The virtiofs share is writable (host-to-guest) but each container's subtree
  is isolated: one container cannot see or modify another container's files
  within the shared tree.
- The network sandbox FD is opened `O_RDONLY | O_CLOEXEC`. It exists solely
  to pin the bind-mount for the CRI-managed netns lifetime; entering that
  netns for VM boot (see [Layer 2](#layer-2--vm-network-stack)) opens its own,
  separate FD on the `netns_path` rather than reusing this one. The shim's
  control-plane goroutines remain in the shim's original network namespace.

## Known limitations

- **Shared `/dev/shm`'s size is set once, by whichever container asks
  first.** The shared tmpfs (see [Shared `/dev/shm`](#shared-devshm)) is
  created the first time any member container needing it is set up, sized
  from that container's own CRI-provided `size=` mount option; a
  later-created sibling with a *different* requested size does not resize
  it, or even surface a warning that its request was ignored — the guest's
  `SharedResources.Create` silently reuses the existing tmpfs (matching
  every other resource type's "created once per id, reused thereafter"
  contract). In practice this is not expected to matter: CRI sends every
  member container of a pod the same `/dev/shm` size, so there is normally
  nothing to disagree about.
- **Mount-namespace crash safety does not cover the apparmor-restricted
  fallback path.** As described in
  [Security properties](#security-properties), a shim that cannot create a
  user namespace due to `apparmor_restrict_unprivileged_userns=1` (and is
  not already real root) runs with no mount namespace isolation at all —
  its mounts are ordinary host mounts from the start. A crash in that
  specific configuration leaves real, host-visible mounts behind, the same
  as it would for any process outside of nerdbox; nothing currently scans
  for and cleans up dangling mounts of this kind on shim startup. In
  practice this only affects unprivileged shim invocations on
  apparmor-restricted hosts — the shim is already skipping mount namespace
  isolation entirely in that case, which is itself an existing, narrower
  gap this doesn't change.
- **An inbound TSI ephemeral bind's resolved port can collide with something
  already bound on the host.** Kernel patch
  `0013-tsi-forward-the-resolved-port-for-ephemeral-binds.patch` (see
  [TSI and guest network namespaces](#tsi-and-guest-network-namespaces))
  makes the guest forward its own resolved port to the host instead of the
  literal `0` requested, so the host now attempts to bind that *specific*
  port rather than picking its own free one. Two outcomes if it's already
  taken, depending on how it's taken:
  - **A normal bind held by something else on the host**: libkrun's TSI
    proxy returns `EADDRINUSE`, and the guest kernel's own `tsi_listen`
    fallback (pre-existing, previously essentially unreachable since a
    host-side `bind(0)` could not fail) transparently switches to a real
    in-guest `listen()` on the same socket. Host-external reachability at
    that port is lost, but the application still gets a working listener,
    and cross-container connectivity within the same VM is unaffected
    (`tsi_connect` tries the in-guest socket first).
  - **A listener from an unrelated VM's own TSI proxy on the same host
    netns**: libkrun sets `SO_REUSEPORT` on TSI's host-side listening
    sockets, so two different VMs' guest kernels — which cannot see each
    other's port allocations and so can genuinely resolve the same
    ephemeral port — both bind successfully, and the host kernel silently
    load-balances inbound connections between two unrelated pods' listeners
    with no error and no fallback triggered. This is only reachable when
    multiple VMs' shims share a host network namespace (e.g. `ctr run`
    without a CNI/pod netns); under CRI/Kubernetes each pod gets its own
    netns containing only that pod's own shim, so this cannot occur in
    practice today. Closing this properly means the host, not the guest,
    should own ephemeral port allocation for TSI listeners — e.g. by
    extending TSI's `tsi_listen_rsp` to report back the port the host
    actually bound, and having the guest adopt it — which needs coordinated
    kernel and libkrun changes; tracked as future work below rather than
    attempted alongside the kernel-only kernel patch `0013` above.

## Future work

The following capabilities are planned but not yet implemented:

- **Turnkey virtio networking** — have the shim spawn and manage a passt or
  gvproxy process (inside the pod netns) rather than requiring a user-supplied
  socket path via annotation.
- **Single ext4 upper layer** — a forthcoming containerd change will support
  placing multiple container upper filesystems in one ext4 image, which can be
  mounted upfront and eliminate per-container mount overhead on non-root hosts.
- **Host-authoritative TSI ephemeral port allocation** — extend libkrun's
  `tsi_listen_rsp` to report back the concrete port the host actually bound
  an inbound listener to, and have the guest kernel adopt it, rather than
  the guest resolving its own ephemeral port and the host attempting to
  match it (see [Known limitations](#known-limitations) for the
  same-host-netns collision this can hit today). A libkrun bump is expected
  soon regardless of this, which is the natural point to carry the
  corresponding host-side change.
