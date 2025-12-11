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

# -----------------------------------------------------------------------------
# syntax=docker/dockerfile:1

# Build the Linux kernel, initrd ,and containerd shim for running nerbox

ARG XX_VERSION=1.9.0
ARG GO_VERSION=1.25.1
ARG BASE_DEBIAN_DISTRO="bookworm"
ARG GOLANG_IMAGE="golang:${GO_VERSION}-${BASE_DEBIAN_DISTRO}"
ARG GOLANGCI_LINT_VERSION=2.5.0
ARG GOLANGCI_FROM_SOURCE=false
ARG DOCKER_VERSION=28.4.0
ARG DOCKER_IMAGE="docker:${DOCKER_VERSION}-cli"
ARG RUST_IMAGE="rust:1.89.0-slim-${BASE_DEBIAN_DISTRO}"

# xx is a helper for cross-compilation
FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx

FROM --platform=$BUILDPLATFORM ${GOLANG_IMAGE} AS base

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
RUN apt-get update && apt-get install --no-install-recommends -y file

FROM base AS kernel-build-base

# Set environment variables for non-interactive installations
ENV DEBIAN_FRONTEND=noninteractive

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

# Install build dependencies
RUN --mount=type=cache,sharing=locked,id=kernel-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=kernel-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y build-essential libncurses-dev flex bison libssl-dev libelf-dev bc cpio git wget xz-utils

ARG KERNEL_VERSION="6.12.46"
ARG KERNEL_ARCH="x86_64"
ARG KERNEL_NPROC="4"

# Set the working directory
WORKDIR /usr/src

RUN wget https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz && \
    tar -xf linux-${KERNEL_VERSION}.tar.xz && \
    rm linux-${KERNEL_VERSION}.tar.xz && \
    mv linux-${KERNEL_VERSION} linux

#COPY --from=config-build /usr/src/fragments/.config /usr/src/linux/.config
COPY kernel/config-${KERNEL_VERSION}-${KERNEL_ARCH} /usr/src/linux/.config

COPY kernel/patches /usr/src/linux/patches
RUN <<EOT
    for patch in $(ls -d /usr/src/linux/patches/*.patch); do
        patch -p1 -d /usr/src/linux < "$patch";
    done
EOT

# Build the kernel
# Seperate from base to allow config construction from fragments in the future
FROM kernel-build-base AS kernel-build

# Compile the kernel
RUN cd linux && make -j${KERNEL_NPROC}

RUN <<EOT
    set -e
    cd linux
    mkdir /build
    case $(uname -m) in
        x86_64) cp vmlinux /build/kernel ;;
        aarch64) cp arch/arm64/boot/Image /build/kernel ;;
        *) echo "Unsupported architecture: $(uname -m)" ; exit 1 ;;
    esac
EOT

FROM base AS shim-build

WORKDIR /go/src/github.com/containerd/nerdbox

ARG GO_DEBUG_GCFLAGS
ARG GO_GCFLAGS
ARG GO_BUILD_FLAGS
ARG GO_LDFLAGS
ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM

RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=shim-build-$TARGETPLATFORM \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build ${GO_DEBUG_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o /build/containerd-shim-nerdbox-v1 ${GO_LDFLAGS} -tags 'no_grpc' ./cmd/containerd-shim-nerdbox-v1

FROM base AS vminit-build

WORKDIR /go/src/github.com/containerd/nerdbox

ARG GO_DEBUG_GCFLAGS
ARG GO_GCFLAGS
ARG GO_BUILD_FLAGS
ARG TARGETPLATFORM

RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=vminit-build-$TARGETPLATFORM \
    go build ${GO_DEBUG_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o /build/vminitd -ldflags '-extldflags \"-static\" -s -w' -tags 'osusergo netgo static_build no_grpc'  ./cmd/vminitd

# TODO: Use nix instructions to build crun statically
#FROM base AS crun-src
#
#ARG CRUN_VERSION=1.24
#
#WORKDIR /usr/src/crun
#RUN git init . && git remote add origin "https://github.com/containers/crun.git"
#
#RUN git fetch -q --depth 1 origin "${CRUN_VERSION}" +refs/tags/*:refs/tags/* && git checkout -q FETCH_HEAD
#
#FROM base AS crun-build
#WORKDIR /go/src/github.com/containers/crun
#ARG TARGETPLATFORM
#RUN --mount=type=cache,sharing=locked,id=crun-aptlib,target=/var/lib/apt \
#    --mount=type=cache,sharing=locked,id=crun-aptcache,target=/var/cache/apt \
#        apt-get update && apt-get install -y --no-install-recommends \
#            make git gcc build-essential pkgconf libtool \
#            libsystemd-dev libprotobuf-c-dev libcap-dev libseccomp-dev libyajl-dev \
#            go-md2man autoconf python3 automake
#RUN --mount=from=crun-src,src=/usr/src/crun,rw \
#    --mount=type=cache,target=/root/.cache/go-build,id=crun-build-$TARGETPLATFORM <<EOT
#  set -e
#  ./autogen.sh
#  ./configure
#  make
#  mkdir /build
#  mv crun /build/
#EOT

FROM base AS crun-build
WORKDIR /usr/src/crun

RUN <<EOT
    mkdir /build
    case $(uname -m) in
        x86_64) ARCH=amd64 ;;
        aarch64) ARCH=arm64 ;;
        *) echo "Unsupported architecture: $(uname -m)" ; exit 1 ;;
    esac
    wget -O /build/crun https://github.com/containers/crun/releases/download/1.24/crun-1.24-linux-${ARCH}-disable-systemd
EOT

FROM base AS initrd-build
WORKDIR /usr/src/init
ARG TARGETPLATFORM
RUN --mount=type=cache,sharing=locked,id=initrd-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=initrd-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends cpio

RUN mkdir sbin proc sys tmp run

COPY --from=vminit-build /build/vminitd ./init
COPY --from=crun-build /build/crun ./sbin/crun

RUN <<EOT
    set -e
    chmod +x sbin/crun
    mkdir /build
    (find . -print0 | cpio --null -H newc -o ) | gzip -9 > /build/nerdbox-initrd
EOT

FROM scratch AS kernel
ARG KERNEL_ARCH="x86_64"
COPY --from=kernel-build /build/kernel /nerdbox-kernel-${KERNEL_ARCH}

FROM scratch AS initrd
COPY --from=initrd-build /build/nerdbox-initrd /nerdbox-initrd

FROM scratch AS shim
COPY --from=shim-build /build/containerd-shim-nerdbox-v1 /containerd-shim-nerdbox-v1

FROM "${DOCKER_IMAGE}" AS docker-cli

FROM "${GOLANG_IMAGE}" AS dlv
RUN go install github.com/go-delve/delve/cmd/dlv@latest

FROM "${RUST_IMAGE}" AS libkrun-build
ARG LIBKRUN_VERSION=v1.15.1

RUN --mount=type=cache,sharing=locked,id=libkrun-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=libkrun-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y git libclang-19-dev llvm make

RUN git clone --depth 1 --branch ${LIBKRUN_VERSION} https://github.com/containers/libkrun.git && \
    cd libkrun && \
    make BLK=1 NET=1

FROM scratch AS libkrun
COPY --from=libkrun-build /libkrun/target/release/libkrun.so /libkrun.so

FROM ${GOLANG_IMAGE} AS dev
ARG CONTAINERD_VERSION=2.1.4
ARG TARGETARCH

ENV PATH=/go/src/github.com/containerd/nerdbox/_output:$PATH
WORKDIR /go/src/github.com/containerd/nerdbox

RUN --mount=type=cache,sharing=locked,id=dev-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=dev-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y erofs-utils git make wget

RUN wget https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz && \
    tar -C /usr/local/bin --strip-components=1 -xf containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz && \
    rm containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz

COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx

COPY --from=dlv /go/bin/dlv /usr/local/bin/dlv

COPY --from=libkrun /libkrun.so /usr/local/lib64/libkrun.so
ENV LIBKRUN_PATH=/go/src/github.com/containerd/nerdbox/_output

VOLUME /var/lib/containerd


FROM base AS golangci-build
WORKDIR /src
ARG GOLANGCI_LINT_VERSION
ADD https://github.com/golangci/golangci-lint.git#v${GOLANGCI_LINT_VERSION} .
COPY --link --from=xx / /
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/ \
  xx-go --wrap && \
  go mod download
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/ \
  xx-go --wrap && \
  mkdir -p out && \
  go build -o /out/golangci-lint ./cmd/golangci-lint

FROM scratch AS golangci-binary-false
FROM scratch AS golangci-binary-true
COPY --from=golangci-build /out/golangci-lint golangci-lint
FROM golangci-binary-${GOLANGCI_FROM_SOURCE} AS golangci-binary

FROM base AS lint-base
ENV GOFLAGS="-buildvcs=false"
RUN <<EOT
apt-get update
apt-get install -y --no-install-recommends gcc libc6-dev yamllint
rm -rf /var/lib/apt/lists/*
EOT
ARG GOLANGCI_LINT_VERSION
ARG GOLANGCI_FROM_SOURCE
COPY --link --from=golangci-binary / /usr/bin/
RUN [ "${GOLANGCI_FROM_SOURCE}" = "true" ] && exit 0; wget -O- -nv https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s v${GOLANGCI_LINT_VERSION}
COPY --link --from=xx / /
WORKDIR /go/src/github.com/containerd/nerdbox

FROM lint-base AS golangci-lint
ARG TARGETNAME
ARG TARGETPLATFORM
RUN --mount=target=/go/src/github.com/containerd/nerdbox \
    --mount=target=/root/.cache,type=cache,id=lint-cache-${TARGETNAME}-${TARGETPLATFORM} \
  xx-go --wrap && \
  golangci-lint run -c .golangci.yml && \
  touch /golangci-lint.done

FROM lint-base AS golangci-verify-false
RUN --mount=target=/go/src/github.com/containerd/nerdbox \
  golangci-lint config verify && \
  touch /golangci-verify.done

FROM scratch AS golangci-verify-true
COPY <<EOF /golangci-verify.done
EOF

FROM golangci-verify-${GOLANGCI_FROM_SOURCE} AS golangci-verify

FROM lint-base AS yamllint
RUN --mount=target=/go/src/github.com/containerd/nerdbox \
  yamllint -c .yamllint.yml --strict . && \
  touch /yamllint.done

FROM scratch AS lint
COPY --link --from=golangci-lint /golangci-lint.done /
COPY --link --from=golangci-verify /golangci-verify.done /
COPY --link --from=yamllint /yamllint.done /
