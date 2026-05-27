# shimtest

A conformance test suite for containerd shim implementations. Tests the task
lifecycle (create, start, exec, kill, delete), stdio round-trip, clock
synchronization across a VM boundary, the transfer service, and UDS socket
forwarding.

## Prerequisites

- A built shim binary (e.g., `containerd-shim-runc-v2` or
  `containerd-shim-nerdbox-v1`)
- Go toolchain (for compiling the test binary and `cmd/testbin`)

No containerd daemon is required. The test rootfs is built in-process from
an embedded Go binary (`cmd/testbin`) compressed as `_output/testbin.gz` and
written into an erofs image at test time.

## Building

```bash
make build
```

This builds `cmd/testbin` (a small multicall binary embedded in the test
rootfs) and compiles the test binary to `_output/shimtest.test`. The testbin
is always cross-compiled for `linux/amd64` via `CGO_ENABLED=0`; override with
`TESTBIN_GOOS`/`TESTBIN_GOARCH` or drop a pre-built `_output/testbin.gz` in
place before `make build`.

## Configuration

Tests are driven by one or more JSON configuration files. See
`shimtest.config.sample.json` for all options. The key fields are:

| Field | Type | Description |
|---|---|---|
| `shim_binary` | string | Name or path of the shim binary to test (required) |
| `uid` | int | UID to run as; defaults to the current user's UID. If set to a value different from the current UID and the effective UID is 0, the harness re-execs itself as that user via `sudo` |
| `gid` | int | GID to run as |
| `format_mounts` | bool | Provide the rootfs as formatted erofs/ext4 images with a `format/mkdir/overlay` descriptor for the shim to mount. Default (`false`) extracts the rootfs and provides a pre-mounted overlay (or plain directory when rootless) |
| `skip` | []string | Feature names to skip (`exec`, `layers`, `oom`, `transfer`, `uds`) |
| `env` | map | Additional environment variables for the test run |
| `debug` | bool | Enable debug logging on the shim |

## Running

shimtest runs in one of two modes.

### Single config

```bash
_output/shimtest.test -test.v -shimtest.config=profiles/myconfig.json
```

### Config directory

All `*.json` files in the directory are loaded; each becomes a subtest named
after the file:

```bash
_output/shimtest.test -test.v -shimtest.configdir=profiles/
```

Configs with a `uid` that doesn't match the current process are skipped, or
re-exec'd via `sudo` when the effective UID is 0. The parent serializes the
full config into a temp file and passes it to the child so no fields are
lost across the re-exec.

### Examples

```bash
# Run a single test case in a single config
_output/shimtest.test -test.v -test.run='TestShim/myconfig/Lifecycle' \
  -shimtest.config=profiles/myconfig.json

# Run all configs in profiles/
sudo _output/shimtest.test -test.v -shimtest.configdir=profiles/

# Run StartupPhases benchmark across all configs (benchtime=3x is quick)
_output/shimtest.test -test.run='^$' \
  -test.bench='BenchmarkShim/[^/]+/StartupPhases' -test.benchtime=3x \
  -shimtest.configdir=profiles/
```

## Tests

All tests are subtests of the single entry point `TestShim`. Within each
config, the tree is `TestShim/<config-name>/<test-name>`.

| Test | Feature | Description |
|---|---|---|
| `Lifecycle` | — | Full create/start/kill/wait/delete cycle |
| `Exec` | exec | Exec a process inside a running container |
| `StdioRoundTrip` | exec | Write to stdin, read from stdout via exec |
| `Clock` | exec | Verify VM clock is synchronized with host |
| `ExitCodes` | exec | Exec processes that exit with a range of status codes and verify propagation |
| `InitExitCodes` | — | Run the container's init process with `/bin/exit N` and verify task-level exit status propagation |
| `OutputThenExit` | — | Run a process that prints 50 lines over 50ms then exits non-zero; verify both exit status and every line of output |
| `FastExitInit` | — | Run the container's init as `/bin/burstexit 8MiB 0`; call Delete immediately after Wait (no drain pause); verify full byte count and CRC-32. Regression test for the close-before-drain race in `io.go` (init path) |
| `Events` | — | Bind a TTRPC events recorder at `TTRPC_ADDRESS` and verify the shim publishes `create`, `start`, `exit`, `delete` events with correct fields |
| `FastExitOutput` | exec | Exec `/bin/burstexit 8MiB 0` and call Delete immediately after Wait; verify full byte count and CRC-32. Regression test for the close-before-drain race in `io.go` (exec path) |
| `LargeFileRead` | exec | Read a 64 MiB fixture from a secondary read-only erofs layer, verify crc32-Castagnoli, report MiB/s |
| `BindMountRead` | exec | Bind-mount the same 64 MiB fixture from a host tempfile and verify+benchmark via the bind path |
| `OOM` | oom | Run a memory hog under a 128MiB limit and verify the kernel OOM-kills it (exit 137) |
| `HundredLayers` | layers | Build a rootfs with 100 stacked erofs layers — layer 1 seeds 99 files in `/base` and adds `/added/file_1`; layers 2..100 each add `/added/file_K` and white out `/base/base_{K-2}`. Verify all 100 added files are present and all 99 base files are gone. Only meaningful with `format_mounts=true` (the shim assembles the multi-layer overlay itself); skipped otherwise |
| `TransferCopyTo` | transfer | Copy a file into a container |
| `TransferCopyToAndFrom` | transfer | Copy a file in and back out |
| `TransferExecVerify` | transfer | Copy a file in, verify via exec |
| `UDSRoundTrip` | uds | UDS socket forwarding round-trip |
| `Stress` | (per feature) | Long-running concurrent stress run. Composes subtests from the enabled features (currently transfer: stat/write/read). Each subtest runs as a goroutine until the test deadline approaches or any one fails (which cancels the rest). Skipped under `-test.short`. |

A separate top-level fuzz target exists alongside `TestShim`:

| Fuzz target | Feature | Description |
|---|---|---|
| `FuzzTransferMissing` | transfer | Fuzzes the transfer service's not-found path. Without `-fuzz` only the seed corpus runs (sub-second); with `-fuzz=FuzzTransferMissing` it generates new inputs continuously until `-fuzztime` elapses or a failing input is found. |

### Planned tests

Candidates to add later, ranked roughly by value:

- **Signals** — send SIGTERM (not SIGKILL) to init, verify exit 143. Most shims get KILL right but botch non-KILL forwarding.
- **Pause/Resume** — pause a ticker process, verify output stops; resume, verify it continues.
- **Stats** — call `tc.Stats()` and assert cgroup counters populate (probe since not all shims implement it).
- **Missing executable** — set `Args` to a nonexistent path and verify a clean error (not a hang or panic).
- **Cold-cache IO** — `LargeFileRead`/`BindMountRead` currently measure warm-cache throughput. A cold-cache variant would need to drop caches between runs (root only) or grow the fixture beyond cache size.
- **Double-kill / post-exit API** — Kill after exit; Wait/State after Delete. Idempotency.
- **Zombie reaping** — init that forks and exits; verify the shim's pid 1 reaps the orphan.

## Benchmarks

A subset of these benchmarks runs against `runc-rootless` and `nerdbox`
on every push to `main` and is published as time-series charts at
<https://dmcgowan.github.io/shimtest/dev/bench/> (gh-pages).


Benchmarks live under `BenchmarkShim/<config-name>/<bench-name>`.

| Benchmark | Feature | Description |
|---|---|---|
| `Lifecycle` | — | Full container create/start/kill/wait/delete cycle |
| `Startup` | — | Shim start through first output |
| `StartupPhases` | — | Same as `Startup` with per-phase breakdown reported via custom metrics (`ms/shim-start`, `ms/connect`, `ms/create`, `ms/task-start`, `ms/output`, `ms/total`) |
| `Start` | — | Shim start subcommand only (bootstrap, no container) |
| `Exec` | exec | Exec cycle inside a running container (exec/start/wait/delete) |
| `StdioRoundTrip` | exec | Stdio write/read at 8B, 4KB, 4MB |
| `UDSRoundTrip` | uds | UDS forwarded-socket throughput in both directions (HostToContainer, ContainerToHost) at 8B, 4KB, 4MB |
| `ThirtyLayers` | layers | Bring up a container with a 30-layer erofs rootfs (same shape as `HundredLayers`, smaller). Reports per-phase metrics (`ms/shim-start`, `ms/create`, `ms/task-start`, `ms/total`) so multi-layer mount overhead can be localized. Requires `format_mounts=true` |

## Using shimtest in your shim's CI

To run shimtest as part of your own shim project's CI, your job needs
to: build your shim, check out and build shimtest, write a JSON profile
that points at your binary, then run `shimtest.test` against it.

### Minimal workflow snippet

```yaml
- name: Build my shim
  run: |
    make my-shim   # produces ./bin/containerd-shim-myshim-v1
    echo "$(pwd)/bin" >> $GITHUB_PATH

- name: Allow unprivileged user namespaces
  # Only needed if your profile uses a non-zero uid on Ubuntu 24.04+.
  run: sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0

- name: Check out shimtest
  uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
  with:
    repository: dmcgowan/shimtest
    ref: <commit-sha>   # pin a commit
    path: shimtest

- name: Setup Go
  uses: actions/setup-go@4a3601121dd01d1626a1e23e37211e3254c1c06c # v6.4.0
  with:
    go-version: "1.26.x"

- name: Build shimtest
  working-directory: shimtest
  run: make build

- name: Write shimtest profile
  run: |
    cat > shimtest/myshim.json <<'EOF'
    {
      "shim_binary": "containerd-shim-myshim-v1",
      "skip": ["transfer", "uds"]
    }
    EOF

- name: Run shimtest
  working-directory: shimtest
  run: |
    _output/shimtest.test -test.v -test.short -test.timeout=300s \
      -shimtest.config=myshim.json
```

The `-test.short` flag opts out of the long-running `Stress` test;
the fuzz target's seed corpus runs either way (it's fast). Drop
`-test.short` from a separate nightly/soak job to exercise the
unbounded `Stress` run, and run active fuzzing as its own step:

```yaml
- name: Run shimtest soak (nightly)
  working-directory: shimtest
  run: |
    _output/shimtest.test -test.v -test.timeout=15m \
      -test.run='TestShim/myshim/Stress' \
      -shimtest.config=myshim.json
    # Active fuzzing in a separate step:
    _output/shimtest.test -test.run='^$' \
      -test.fuzz=FuzzTransferMissing -test.fuzztime=10m \
      -test.fuzzcachedir=$RUNNER_TEMP/fuzzcache \
      -shimtest.config=myshim.json
```

### What you'll need to configure

- **`shim_binary`**: bare name (resolved via `PATH`) or absolute path.
  shimtest also adds the binary's directory to `PATH` so sibling
  helpers (kernels, libraries) co-located with the shim resolve.
- **`format_mounts`**: set `true` if your shim mounts the rootfs
  itself from `format/mkdir/overlay` descriptors (VM-based shims);
  leave `false` to receive a pre-mounted overlay or plain directory.
- **`uid`**: omit to run as the runner user. Set explicitly when you
  want the harness to `sudo` re-exec itself or rewrite the profile.
- **`skip`**: list of feature names to disable. Currently meaningful
  values are `exec`, `layers`, `oom`, `transfer`, and `uds` — useful
  when your shim doesn't implement transfer/UDS forwarding,
  multi-layer rootfs descriptors, or when running rootless without
  cgroup delegation.

### Multiple configs

If you want one job to test several profiles (e.g., rootless and root
variants of the same shim) put the JSON files under a directory and
pass `-shimtest.configdir=` instead of `-shimtest.config=`. Each file
becomes a `TestShim/<filename>/...` subtest. Profiles whose `uid`
differs from the running process are skipped, or `sudo`-re-exec'd
when the harness is running as root.

### Benchmarks

The benchmark binary is the same as the test binary — just point
`-test.bench` at the suite and disable tests:

```yaml
- name: Run benchmarks
  working-directory: shimtest
  run: |
    _output/shimtest.test -test.run='^$' \
      -test.bench='BenchmarkShim/myshim/(Lifecycle|Startup|Exec)' \
      -test.benchtime=5x \
      -shimtest.config=myshim.json | tee bench.txt
```
