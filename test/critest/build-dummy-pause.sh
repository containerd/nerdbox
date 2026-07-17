#!/usr/bin/env bash

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

# build-dummy-pause.sh builds a deliberately non-functional OCI image and
# writes it as an importable tar (OCI image layout) to $OUT (default:
# ./dummy-pause.tar next to this script).
#
# Why a dummy image at all: CRI's RunPodSandbox unconditionally calls
# ensurePauseImageExists() before starting the sandbox, regardless of which
# sandboxer is configured. On the *shim* sandboxer path (which is what
# nerdbox uses — see docs/sandbox-architecture.md), the pause image is never
# actually mounted or run: CRI's sandbox_run.go only calls
# sandbox.WithOptions/WithNetNSPath when creating the sandbox, never
# WithRootFS, so CreateSandboxRequest.Rootfs arrives empty at the shim.
# ensurePauseImageExists only needs the ref to *resolve locally* in
# containerd's image store (a manifest + config blob reachable in the
# content store) — it does not need to be pulled, unpacked, or runnable.
#
# Why deliberately non-functional (no /pause binary, empty layer): if
# anything ever DID try to actually run this image, it would fail loudly
# instead of silently working — proof that nerdbox's shim-sandbox path
# truly does not depend on the pause image.
#
# Usage: build-dummy-pause.sh [output-tar-path] [image-ref]
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" > /dev/null 2>&1; pwd -P)"
OUT="${1:-${SCRIPT_DIR}/dummy-pause.tar}"
REF="${2:-nerdbox.local/dummy-pause:1}"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

OCIDIR="${WORK}/oci"
BLOBS="${OCIDIR}/blobs/sha256"
mkdir -p "${BLOBS}"

echo '{"imageLayoutVersion": "1.0.0"}' > "${OCIDIR}/oci-layout"

# --- Empty layer: a well-formed, but empty, tar+gzip. Real tar/gzip tools
# so the blob is format-valid (in case any tooling ever inspects it), while
# containing zero files — nothing to unpack, nothing to execute.
LAYER_TAR="${WORK}/layer.tar"
tar --create --file="${LAYER_TAR}" --files-from=/dev/null
DIFF_ID="sha256:$(sha256sum "${LAYER_TAR}" | awk '{print $1}')"

LAYER_GZ="${WORK}/layer.tar.gz"
gzip -n -c "${LAYER_TAR}" > "${LAYER_GZ}"
LAYER_DIGEST="$(sha256sum "${LAYER_GZ}" | awk '{print $1}')"
LAYER_SIZE="$(stat -c%s "${LAYER_GZ}")"
cp "${LAYER_GZ}" "${BLOBS}/${LAYER_DIGEST}"

# --- Image config: minimal valid OCI image config. No Entrypoint/Cmd —
# there is nothing in the (empty) rootfs to exec anyway.
CONFIG_JSON="${WORK}/config.json"
cat > "${CONFIG_JSON}" <<EOF
{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":["${DIFF_ID}"]}}
EOF
CONFIG_DIGEST="$(sha256sum "${CONFIG_JSON}" | awk '{print $1}')"
CONFIG_SIZE="$(stat -c%s "${CONFIG_JSON}")"
cp "${CONFIG_JSON}" "${BLOBS}/${CONFIG_DIGEST}"

# --- Manifest: references the config and the single empty layer.
MANIFEST_JSON="${WORK}/manifest.json"
cat > "${MANIFEST_JSON}" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:${CONFIG_DIGEST}","size":${CONFIG_SIZE}},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:${LAYER_DIGEST}","size":${LAYER_SIZE}}]}
EOF
MANIFEST_DIGEST="$(sha256sum "${MANIFEST_JSON}" | awk '{print $1}')"
MANIFEST_SIZE="$(stat -c%s "${MANIFEST_JSON}")"
cp "${MANIFEST_JSON}" "${BLOBS}/${MANIFEST_DIGEST}"

# --- Index: names the manifest with the ref this script imports it as, so
# "ctr images import" tags it automatically.
INDEX_JSON="${OCIDIR}/index.json"
cat > "${INDEX_JSON}" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:${MANIFEST_DIGEST}","size":${MANIFEST_SIZE},"platform":{"architecture":"amd64","os":"linux"},"annotations":{"org.opencontainers.image.ref.name":"${REF}"}}]}
EOF

# --- Package as an importable tar (ctr images import consumes an OCI image
# layout tar stream directly).
tar --create --file="${OUT}" -C "${OCIDIR}" oci-layout index.json blobs

echo "wrote ${OUT} (ref: ${REF})"
