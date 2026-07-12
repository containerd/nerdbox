#!/usr/bin/env bash
#
#   Copyright The containerd Authors.
#
#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.
#
# run-critest.sh drives a dedicated containerd instance, configured with a
# "nerdbox" CRI runtime handler (runtime_type = io.containerd.nerdbox.v1,
# sandboxer = "shim" — the shim sandboxer, NOT the podsandbox controller),
# through smoke tests and/or the full critest (CRI conformance) suite.
#
# See README.md in this directory for prerequisites and an explanation of
# every path/env var this script uses.
#
# Usage:
#   run-critest.sh up               # generate config, start containerd, leave it running
#   run-critest.sh down             # stop the containerd started by "up"
#   run-critest.sh smoke            # up -> crictl lifecycle smoke test -> down (always)
#   run-critest.sh critest [ARGS]   # up -> critest --runtime-handler=nerdbox [ARGS] -> down (always)
#   run-critest.sh shell            # up, then drop into a shell with env set for manual crictl use
#
# ARGS are passed straight through to the critest binary's own (ginkgo/go
# test) flag parser — do NOT prepend a literal "--" separator: a bare "--"
# is itself consumed by that parser as "stop parsing flags", which silently
# disables every flag after it (including --ginkgo.focus/--ginkgo.skip).
# e.g.: run-critest.sh critest --ginkgo.focus="HostPID"
#
# By default, "critest" skips a small, fixed set of specs that are known,
# permanent architectural limitations of running each sandbox in its own VM
# (not implementation bugs) — see README.md's "Known conformance gaps" for
# what they are and why. Pass --no-skip to run the full, unfiltered suite
# and see them fail. Note --no-skip and --ginkgo.focus/--ginkgo.skip compose
# via ginkgo's normal flag semantics: if ARGS also specifies --ginkgo.skip,
# that value applies (skipping is not additionally layered in that case).
#
# Env vars (all optional, defaults shown):
#   NERDBOX_OUTPUT_DIR   repo _output/ dir (shim, kernel, rootfs, libkrun.so)   [<repo>/_output]
#   CONTAINERD_BIN       path to a containerd binary (built from source)       [containerd on PATH]
#   CTR_BIN              path to ctr                                          [ctr on PATH]
#   CRICTL_BIN           path to crictl                                       [crictl on PATH]
#   CRITEST_BIN          path to critest                                      [critest on PATH]
#   CNI_BIN_DIR          directory with bridge/loopback/host-local/portmap    [/opt/cni/bin]
#   RUNTIME_HANDLER      CRI runtime handler to exercise                      [nerdbox]
#   WORK_DIR             scratch dir for root/state/socket/logs/CNI conf      [<here>/.work]
#   KEEP_WORK_DIR        if set to 1, don't delete WORK_DIR content on "down"
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" > /dev/null 2>&1; pwd -P)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." > /dev/null 2>&1; pwd -P)"

NERDBOX_OUTPUT_DIR="${NERDBOX_OUTPUT_DIR:-${REPO_ROOT}/_output}"
CONTAINERD_BIN="${CONTAINERD_BIN:-containerd}"
CTR_BIN="${CTR_BIN:-ctr}"
CRICTL_BIN="${CRICTL_BIN:-crictl}"
CRITEST_BIN="${CRITEST_BIN:-critest}"
CNI_BIN_DIR="${CNI_BIN_DIR:-/opt/cni/bin}"
RUNTIME_HANDLER="${RUNTIME_HANDLER:-nerdbox}"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.work}"
KEEP_WORK_DIR="${KEEP_WORK_DIR:-0}"

SOCK="${WORK_DIR}/c.sock"
PIDFILE="${WORK_DIR}/containerd.pid"
CONFIG="${WORK_DIR}/config.toml"
CNI_CONF_DIR="${WORK_DIR}/cni/net.d"
DUMMY_PAUSE_TAR="${WORK_DIR}/dummy-pause.tar"
DUMMY_PAUSE_REF="nerdbox.local/dummy-pause:1"

log() { echo "[run-critest] $*" >&2; }
die() { log "ERROR: $*"; exit 1; }

require_bin() {
	local name="$1" path="$2"
	if [[ "${path}" == */* ]]; then
		[[ -x "${path}" ]] || die "$name not found or not executable: ${path}"
	else
		command -v "${path}" > /dev/null 2>&1 || die "$name not found on PATH: ${path} (set ${name^^}_BIN or add it to PATH)"
	fi
}

check_prereqs() {
	require_bin containerd "${CONTAINERD_BIN}"
	require_bin ctr "${CTR_BIN}"
	require_bin crictl "${CRICTL_BIN}"
	require_bin jq jq
	[[ -c /dev/kvm ]] || die "/dev/kvm not found; the nerdbox shim needs KVM"
	for f in containerd-shim-nerdbox-v1 nerdbox-kernel-x86_64 nerdbox-rootfs.erofs libkrun.so; do
		[[ -e "${NERDBOX_OUTPUT_DIR}/${f}" ]] || die "missing ${f} in NERDBOX_OUTPUT_DIR=${NERDBOX_OUTPUT_DIR} (build it first, see README.md)"
	done
	for p in bridge loopback host-local portmap; do
		[[ -x "${CNI_BIN_DIR}/${p}" ]] || die "missing CNI plugin ${p} in CNI_BIN_DIR=${CNI_BIN_DIR}"
	done
}

gen_cni_conf() {
	mkdir -p "${CNI_CONF_DIR}"
	cp "${SCRIPT_DIR}/cni-net.conflist" "${CNI_CONF_DIR}/10-nerdbox-critest.conflist"
}

gen_dummy_pause() {
	[[ -f "${DUMMY_PAUSE_TAR}" ]] || "${SCRIPT_DIR}/build-dummy-pause.sh" "${DUMMY_PAUSE_TAR}" "${DUMMY_PAUSE_REF}"
}

gen_config() {
	mkdir -p "${WORK_DIR}/root" "${WORK_DIR}/state"
	cat > "${CONFIG}" <<EOF
version = 3
root = "${WORK_DIR}/root"
state = "${WORK_DIR}/state"

[grpc]
  address = "${SOCK}"

# --- CRI image service: global default snapshotter (used by the "runc"
# comparison runtime and any pull not scoped to a runtime override), plus
# the dummy pause image pinned as the sandbox image. See
# build-dummy-pause.sh for why this is deliberately non-functional.
[plugins.'io.containerd.cri.v1.images']
  snapshotter = "overlayfs"
  [plugins.'io.containerd.cri.v1.images'.pinned_images]
    sandbox = "${DUMMY_PAUSE_REF}"

# --- CRI runtime service: two runtime handlers.
#   "runc"    - default, unchanged, overlayfs. Kept as a comparison/fallback
#               runtime so most non-VM-specific CRI behavior has a known-good
#               baseline alongside nerdbox.
#   "nerdbox" - the runtime under test. sandboxer = "shim" selects
#               containerd's built-in shim sandboxer (RemoteController over
#               the shim's TTRPC sandbox service) rather than the podsandbox
#               controller — see docs/sandbox-architecture.md. snapshotter =
#               "erofs" matches nerdbox's host-side rootfs assembly, which
#               only ever produces erofs/overlay mounts.
[plugins.'io.containerd.cri.v1.runtime']
  [plugins.'io.containerd.cri.v1.runtime'.containerd]
    default_runtime_name = "runc"
    [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
      runtime_type = "io.containerd.runc.v2"
    [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.${RUNTIME_HANDLER}]
      runtime_type = "io.containerd.nerdbox.v1"
      sandboxer = "shim"
      snapshotter = "erofs"
  [plugins.'io.containerd.cri.v1.runtime'.cni]
    bin_dirs = ["${CNI_BIN_DIR}"]
    conf_dir = "${CNI_CONF_DIR}"
EOF
}

start_containerd() {
	[[ -f "${PIDFILE}" ]] && kill -0 "$(cat "${PIDFILE}")" 2>/dev/null && die "containerd already running (pid $(cat "${PIDFILE}")); run 'down' first"

	mkdir -p "${WORK_DIR}"
	check_prereqs
	gen_cni_conf
	gen_dummy_pause
	gen_config

	log "starting containerd (log: ${WORK_DIR}/containerd.log)"
	# PATH must carry the nerdbox artifacts (shim binary, libkrun.so, kernel,
	# rootfs) so internal/vm/libkrun's PATH/LIBKRUN_PATH search finds them —
	# see internal/vm/libkrun/instance.go.
	PATH="${NERDBOX_OUTPUT_DIR}:${PATH}" \
		setsid "${CONTAINERD_BIN}" --config "${CONFIG}" \
		> "${WORK_DIR}/containerd.log" 2>&1 < /dev/null &
	echo $! > "${PIDFILE}"
	disown || true

	log "waiting for ${SOCK}"
	for _ in $(seq 1 100); do
		[[ -S "${SOCK}" ]] && "${CTR_BIN}" --address "${SOCK}" version > /dev/null 2>&1 && break
		sleep 0.2
	done
	"${CTR_BIN}" --address "${SOCK}" version > /dev/null 2>&1 || die "containerd did not become ready; see ${WORK_DIR}/containerd.log"

	log "importing dummy pause image into k8s.io namespace"
	"${CTR_BIN}" --address "${SOCK}" -n k8s.io images import "${DUMMY_PAUSE_TAR}" > /dev/null

	log "up: pid=$(cat "${PIDFILE}") sock=${SOCK}"
}

stop_containerd() {
	if [[ -f "${PIDFILE}" ]]; then
		local pid
		pid="$(cat "${PIDFILE}")"
		if kill -0 "${pid}" 2>/dev/null; then
			log "stopping containerd (pid ${pid})"
			kill "${pid}" 2>/dev/null || true
			for _ in $(seq 1 50); do
				kill -0 "${pid}" 2>/dev/null || break
				sleep 0.2
			done
			kill -9 "${pid}" 2>/dev/null || true
		fi
		rm -f "${PIDFILE}"
	fi
	# Best-effort: reap any leaked nerdbox shim/VM processes from this run.
	pkill -9 -f "containerd-shim-nerdbox-v1.*${SOCK}" 2>/dev/null || true

	if [[ "${KEEP_WORK_DIR}" != "1" ]]; then
		rm -rf "${WORK_DIR}/root" "${WORK_DIR}/state"
	fi
}

crictl_() {
	"${CRICTL_BIN}" --runtime-endpoint "unix://${SOCK}" --image-endpoint "unix://${SOCK}" "$@"
}

cmd_smoke() {
	local sandbox_json container_json podid cid out

	sandbox_json="${WORK_DIR}/smoke-sandbox.json"
	container_json="${WORK_DIR}/smoke-container.json"

	cat > "${sandbox_json}" <<EOF
{
  "metadata": {"name": "nerdbox-smoke", "namespace": "default", "uid": "nerdbox-smoke-uid"},
  "logDirectory": "${WORK_DIR}/pod-logs",
  "linux": {}
}
EOF
	mkdir -p "${WORK_DIR}/pod-logs"

	log "RunPodSandbox (runtimeHandler=${RUNTIME_HANDLER})"
	podid="$(crictl_ runp --runtime "${RUNTIME_HANDLER}" "${sandbox_json}")"
	log "pod sandbox: ${podid}"

	cat > "${container_json}" <<'EOF'
{
  "metadata": {"name": "smoke"},
  "image": {"image": "docker.io/library/busybox:latest"},
  "command": ["sleep", "3600"],
  "log_path": "smoke.log"
}
EOF

	# --with-pull: the image is pulled as part of CreateContainer, scoped to
	# this pod's sandbox (podid) — which resolves to the "nerdbox" runtime's
	# snapshotter (erofs) via CRIImageService.RuntimeSnapshotter, the same
	# path container_create.go uses for the real snapshot/mount setup. No
	# separate "crictl pull" step (and no --runtime-platform flag, which
	# doesn't exist) is needed.
	log "CreateContainer (pulls busybox via the nerdbox/erofs snapshotter)"
	cid="$(crictl_ create --with-pull "${podid}" "${container_json}" "${sandbox_json}")"
	log "container: ${cid}"

	log "StartContainer"
	crictl_ start "${cid}"

	log "ExecSync"
	out="$(crictl_ exec "${cid}" echo smoke-ok)"
	[[ "${out}" == *smoke-ok* ]] || die "unexpected exec output: ${out}"
	log "exec output: ${out}"

	log "container/pod status"
	crictl_ inspect "${cid}" > "${WORK_DIR}/smoke-container-inspect.json"
	crictl_ inspectp "${podid}" > "${WORK_DIR}/smoke-pod-inspect.json"

	# --- Verify the shim-sandboxer path was actually used, and that the
	# dummy pause image is exactly as inert as intended: CRI's
	# ensurePauseImageExists only needs it to *resolve*, and the shim
	# sandboxer never mounts a sandbox rootfs at all (see
	# build-dummy-pause.sh and docs/sandbox-architecture.md). Confirm both:
	local pod_snapshotter pod_snapshot_key
	pod_snapshotter="$(jq -r '.info.snapshotter // ""' < "${WORK_DIR}/smoke-pod-inspect.json")"
	pod_snapshot_key="$(jq -r '.info.snapshotKey // ""' < "${WORK_DIR}/smoke-pod-inspect.json")"
	if [[ -n "${pod_snapshotter}" || -n "${pod_snapshot_key}" ]]; then
		die "pod sandbox unexpectedly has a snapshotter/rootfs (snapshotter=${pod_snapshotter} snapshotKey=${pod_snapshot_key}); expected empty on the shim-sandboxer path"
	fi
	log "confirmed: pod sandbox has no snapshotter/rootfs (shim-sandboxer path, not podsandbox)"

	if "${CTR_BIN}" --address "${SOCK}" -n k8s.io snapshots --snapshotter erofs ls 2>/dev/null | grep -q "${DUMMY_PAUSE_REF}"; then
		die "dummy pause image was unexpectedly unpacked (a snapshot exists for it)"
	fi
	log "confirmed: dummy pause image was never unpacked (no snapshot exists for it)"

	log "StopContainer / RemoveContainer / StopPodSandbox / RemovePodSandbox"
	crictl_ stop "${cid}"
	crictl_ rm "${cid}"
	crictl_ stopp "${podid}"
	crictl_ rmp "${podid}"

	log "SMOKE TEST PASSED"
}

# DEFAULT_SKIP_SPECS are critest specs that are known, permanent
# architectural limitations of running each sandbox in its own VM kernel —
# not implementation bugs — so they are skipped by default. Each one's
# setup mutates state on the literal machine `critest` runs on (the real
# host) and then expects a container running inside a *different* kernel
# (the guest) to observe that mutation, which a VM-isolated runtime cannot
# ever do without abandoning that isolation. See README.md's "Known
# conformance gaps" for the detailed root-cause analysis of each one.
DEFAULT_SKIP_SPECS=(
	"runtime should support HostIpc is true"
	"runtime should support HostNetwork is true"
	"mount with 'rshared' should support propagation from host to container and vice versa"
	"should support non-recursive readonly mounts"
)

join_regex() {
	local IFS='|'
	echo "(${*})"
}

cmd_critest() {
	local no_skip=0
	local args=()
	for a in "$@"; do
		if [[ "${a}" == "--no-skip" ]]; then
			no_skip=1
		else
			args+=("${a}")
		fi
	done

	local skip_flag=()
	if [[ "${no_skip}" != "1" ]]; then
		skip_flag=(--ginkgo.skip="$(join_regex "${DEFAULT_SKIP_SPECS[@]}")")
		log "skipping ${#DEFAULT_SKIP_SPECS[@]} known architectural-limitation specs (see README.md; pass --no-skip to run them anyway)"
	fi

	log "running critest --runtime-handler=${RUNTIME_HANDLER}"
	"${CRITEST_BIN}" \
		--runtime-endpoint "unix://${SOCK}" \
		--image-endpoint "unix://${SOCK}" \
		--runtime-handler "${RUNTIME_HANDLER}" \
		--report-dir "${WORK_DIR}/critest-report" \
		"${skip_flag[@]}" \
		"${args[@]}"
}

main() {
	local sub="${1:-}"
	[[ $# -gt 0 ]] && shift || true

	case "${sub}" in
	up)
		start_containerd
		;;
	down)
		stop_containerd
		;;
	smoke)
		trap stop_containerd EXIT
		start_containerd
		cmd_smoke
		;;
	critest)
		trap stop_containerd EXIT
		start_containerd
		cmd_critest "$@"
		;;
	shell)
		start_containerd
		log "environment ready; sock=${SOCK}"
		log "example: crictl --runtime-endpoint unix://${SOCK} --image-endpoint unix://${SOCK} info"
		CRICTL_SOCK="${SOCK}" bash -i
		;;
	*)
		die "usage: $0 {up|down|smoke|critest [ARGS]|shell}"
		;;
	esac
}

main "$@"
