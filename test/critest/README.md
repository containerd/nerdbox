# CRI conformance harness (critest)

This directory drives a dedicated containerd instance, configured with a
**RuntimeClass-style runtime handler** that uses this shim through
containerd's built-in **shim sandboxer** (`sandboxer = "shim"`, *not* the
`podsandbox` controller), through smoke tests and the full
[critest](https://github.com/kubernetes-sigs/cri-tools) (CRI conformance)
suite.

See `docs/sandbox-architecture.md` for background on the shim sandboxer vs.
podsandbox distinction, and why this matters for the nerdbox shim.

## Why a runtime handler + shim sandboxer, and not the default podsandbox path

containerd's CRI plugin supports two ways to run a pod sandbox:

- **podsandbox** (default): containerd's CRI layer builds the sandbox's OCI
  spec itself and runs a real "pause" container for it via the ordinary
  shim-v2 task API.
- **shim** (what this harness configures): containerd hands the sandbox
  lifecycle entirely to the shim's own TTRPC sandbox controller
  (`CreateSandbox`/`StartSandbox`/`StopSandbox`/`ShutdownSandbox`). This is
  the API nerdbox actually implements (`internal/shim/sandbox/service.go`) —
  one VM per pod, with member containers created afterward over the shim-v2
  task API on the same TTRPC connection.

The runtime handler is configured with `sandboxer = "shim"` in
`run-critest.sh`'s generated `config.toml`.

## Prerequisites

- Linux host with `/dev/kvm` accessible.
- The nerdbox artifacts built into `_output/` at the repo root:
  `containerd-shim-nerdbox-v1`, `nerdbox-kernel-x86_64`,
  `nerdbox-rootfs.erofs`, `libkrun.so`. Build with:
  ```
  task build:shim
  DESTDIR=_output docker buildx bake kernel rootfs libkrun
  ```
- A **containerd binary built from source at the version pinned in
  `go.mod`** (v2.3.2 as of writing) — not a distro package, and not an
  older prebuilt release: the CRI plugin's config schema
  (`[plugins.'io.containerd.cri.v1.runtime']`, split from
  `io.containerd.cri.v1.images`) is version-specific.
  ```
  git clone --branch v2.3.2 https://github.com/containerd/containerd.git
  cd containerd && make binaries   # produces bin/containerd, bin/ctr
  ```
- `crictl` and `critest` from
  [cri-tools](https://github.com/kubernetes-sigs/cri-tools), built at the
  version containerd itself pins for testing
  (`script/setup/critools-version` in the containerd source, v1.35.0 as of
  writing):
  ```
  git clone --branch v1.35.0 https://github.com/kubernetes-sigs/cri-tools.git
  cd cri-tools && make binaries    # produces build/bin/linux/amd64/{crictl,critest}
  ```
- Standard CNI plugins (`bridge`, `loopback`, `host-local`, `portmap`) —
  typically already present at `/opt/cni/bin` on a host that has ever run
  Kubernetes or a CNI-based container runtime. Get them from
  [containernetworking/plugins](https://github.com/containernetworking/plugins)
  releases otherwise.
- `jq` (used by the smoke test to inspect pod status JSON).

## Usage

Point the script at your built tools via env vars (or put them on `PATH`),
then run one of the subcommands:

```sh
export CONTAINERD_BIN=/path/to/containerd/bin/containerd
export CTR_BIN=/path/to/containerd/bin/ctr
export CRICTL_BIN=/path/to/cri-tools/build/bin/linux/amd64/crictl
export CRITEST_BIN=/path/to/cri-tools/build/bin/linux/amd64/critest

sudo -E env PATH="$PATH" \
  CONTAINERD_BIN="$CONTAINERD_BIN" CTR_BIN="$CTR_BIN" \
  CRICTL_BIN="$CRICTL_BIN" CRITEST_BIN="$CRITEST_BIN" \
  ./run-critest.sh smoke      # quick end-to-end sanity check
```

```sh
# same env, then:
./run-critest.sh critest      # full CRI conformance suite
./run-critest.sh up           # start containerd and leave it running
./run-critest.sh shell        # start containerd, drop into a shell to poke at it with crictl
./run-critest.sh down         # stop whatever "up" started
```

`critest` accepts extra args, passed straight through to the critest
binary's own (ginkgo/go test) flag parser — do not prepend a literal `--`
separator; ginkgo's flag parser itself treats a bare `--` as "stop parsing
flags", which silently disables everything after it, including
`--ginkgo.focus`/`--ginkgo.skip`:

```sh
./run-critest.sh critest --ginkgo.focus="HostPID"   # correct
./run-critest.sh critest -- --ginkgo.focus="HostPID"  # wrong: focus silently ignored
```

By default, `critest` skips the 4 specs described in "Known conformance
gaps" below (permanent architectural limitations, not bugs). Pass
`--no-skip` to run the full, unfiltered suite and see them fail:

```sh
./run-critest.sh critest --no-skip
```

Note: if you also pass your own `--ginkgo.focus` and it happens to match
one of the 4 default-skipped specs, you need `--no-skip` too, or it will
match zero specs.

`sudo` is required: containerd's default root/state dirs and the CNI
bridge setup need it, matching how `crictl`/CRI integration tests are
normally run (see containerd's own `script/critest.sh` /
`script/test/cri-integration.sh` for the same pattern).

Everything scratch-state lives under `test/critest/.work/` (gitignored):
`config.toml`, containerd's `root`/`state`, the containerd log, the dummy
pause image tar, CNI conf, and (for `smoke`) captured pod/container status
JSON. Inspect `.work/containerd.log` and `.work/critest-report/` after a
run.

## The dummy pause image

CRI's `RunPodSandbox` unconditionally calls `ensurePauseImageExists()`
before starting the sandbox, regardless of which sandboxer is configured.
On the shim-sandboxer path, however, the pause image is never actually
used: containerd's CRI `sandbox_run.go` only calls
`sandbox.WithOptions`/`WithNetNSPath` when creating the sandbox, never
`WithRootFS`, so the pause image only needs to *resolve* in containerd's
image store — it is never pulled by weight, unpacked, or run.

`build-dummy-pause.sh` builds a deliberately non-functional OCI image (a
valid manifest + config, but an empty layer — no `/pause` binary, nothing
to execute) and `run-critest.sh` imports it under a pinned CRI
`sandbox_image` ref. Using a non-functional image is intentional: if
anything ever did try to actually run it, it would fail loudly instead of
silently working, which is a running proof that this shim's sandbox path
truly doesn't depend on it. The smoke test asserts this explicitly (no
snapshot is ever created for the dummy image, and the pod sandbox status
reports an empty `snapshotter`/`snapshotKey`).

## Known conformance gaps

A first full `critest` run found and fixed two real shim bugs blocking CRI
use entirely (see git history for `pkg/shim/manager` and
`internal/shim/sandbox/service.go` around this harness's introduction: a
missing-`config.json` crash at shim `Start`, and `SandboxStatus.State` not
matching the CRI `PodSandboxState` enum names), then a further round fixed
host bind-mount volumes for member containers, DNS config, hostname, and
sysctls (see git history for `internal/shim/sandbox/sharedfs.go`'s
`ShareVolume`, `internal/shim/task/sandboxvolumes.go`, and
`internal/shim/task/podconfig.go`), then a further round added pod-level
PID and IPC namespace sharing between member containers (see git history
for `internal/podns`, `internal/vminit/podns`, `internal/vminit/podpause`,
and `internal/shim/task/podnetns.go`'s rewritten `sanitizeNamespaces`).

**Current status (`--no-skip`, the full unfiltered suite): 85 passed / 4
failed / 24 skipped.** (With the default skip list applied: 85 passed / 0
failed / 28 skipped.) All 4 remaining failures are **genuine architectural
limitations** of the current design, not bugs, and are not expected to be
fixed without a fundamentally different sharing mechanism:

- **`mount with 'rshared' should support propagation from host to
  container and vice versa`**: this test checks *two* directions, and only
  one of them actually fails. **Host→container works**: the test's own
  setup (`createHostPathForMountPropagation`) explicitly bind-mounts the
  volume's host source onto itself and marks it `MS_SHARED`, and per
  `mount_namespaces(7)`, a later bind mount taken *from* an already-shared
  mount joins the same peer group — which is exactly what
  `SharedFS.ShareVolume`'s own (plain, non-private) bind mount does. So a
  mount created on the host under the volume's source dir *after* the
  container starts lands in the same host-kernel peer group as our
  virtiofs-shared copy, and virtiofs (a live FUSE content server, not a
  point-in-time snapshot) simply serves the now-updated content — this is
  confirmed by the sibling test `mount with 'rslave' should support
  propagation from host to container`, which tests only this direction
  and **passes**. **Container→host is what actually fails**: a mount the
  *container* creates (`mount --bind /etc containerMntPoint`, run inside
  the guest) is a guest-kernel-internal operation. Virtio-fs's protocol
  has no message for "a mount happened" — it only relays file/directory
  *content* operations (open/read/readdir/etc.) — so there is no path by
  which a guest-side `mount(2)` syscall could ever be observed by the host
  kernel, regardless of any peer-group configuration on the host side.
  This is a one-way, permanent limitation of the container→host direction
  specifically, not of virtiofs-based propagation as a whole.
- **`should support non-recursive readonly mounts`**: this test mounts a
  *separate, real* tmpfs on the host, nested inside a volume's source
  directory, *before* the container starts (not a live-propagation
  scenario — the nested mount already exists when `ShareVolume`'s
  recursive (`rbind`) host-side bind mount runs), and expects the OCI
  runtime to recognize the nested mount as a distinct kernel object and
  leave its own read-write flag alone when the container's own bind mount
  is non-recursive. `rbind` does duplicate the nested tmpfs as its own
  mount object in our host-side copy — but virtiofs (like most tree-share
  protocols) does not cross mount points while serving a shared directory
  to the guest, so the guest simply sees `/mnt/tmpfs` as an ordinary
  (flattened) subdirectory of `/mnt`, with no mount boundary at all. From
  crun's point of view inside the guest there is only one mount to apply
  non-recursive-readonly to, so `/mnt/tmpfs` inherits it along with
  everything else. Related to, but distinct from, the `rshared` case
  above: that one is about a guest-created mount never reaching the host;
  this one is about a host-side nested mount boundary never reaching the
  guest as a distinct mount object in the first place.
- **`runtime should support HostNetwork is true`**: this test runs
  `netstat -ln` inside the container and expects the *host's own listening
  socket* to literally appear in the output — true, introspectable network
  stack sharing (the container sees the same socket table as the host),
  not just outbound reachability. TSI (this shim's default outbound
  networking — see docs/sandbox-architecture.md) proxies individual
  outbound connections over vsock; it does not mirror the host's socket
  table into the guest, so nothing the shim does with network namespaces
  can satisfy this specific check.
- **`runtime should support HostIpc is true`**: this test creates a SysV
  shared memory segment directly on the machine running `critest` (the
  *real* host), before creating the pod sandbox, then expects a container
  with `HostIpc: true` to see it. This shim runs every sandbox inside a
  VM, so "the host" from the guest kernel's point of view is the guest's
  own root IPC namespace — a different kernel instance entirely from the
  machine `critest` is actually creating shm segments on. No IPC namespace
  configuration inside the guest can make a segment that only exists in
  the real host kernel visible there; it is the same category of
  limitation as `HostNetwork is true` above (the guest is not the literal
  host), just for SysV IPC instead of the socket table. Pod-level IPC
  sharing *between member containers of the same sandbox* — the far more
  common Kubernetes use case (pods share IPC by default) — works
  correctly and is covered by shimtest's `MemberContainersShareIPC`.

These 4 are wired into `run-critest.sh`'s `DEFAULT_SKIP_SPECS`, which
`critest` applies by default (pass `--no-skip` to see them fail) — see
"Usage" above. Keep that list and this section in sync if either changes;
ask before assuming any *other* failure is out of scope for follow-up
work.

For comparison: Kata Containers, the most mature production VM-isolated
CRI runtime, does not run the upstream `critest` `[k8s.io]` validation
suite in CI at all. Its containerd `cri-integration` job uses an explicit
*allowlist* of the handful of Go tests it knows pass
(`FOCUS="^(TestContainerStats|TestImageLoad|...)$"`), each exclusion
documented inline with its own rationale (e.g. its `TestContainerRestart`
exclusion notes that starting a new container in an already-torn-down
sandbox VM "has never been supported by kata-containers"). That is the
same category of reasoning as the 4 specs here: tests that assume
shared-kernel/host-visibility semantics no VM-isolated runtime can
provide, excluded and documented rather than chased as bugs.
