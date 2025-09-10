# Build the Linux kernel, initrd ,and containerd shim for running nerbox

ARG GO_VERSION=1.25.1
ARG BASE_DEBIAN_DISTRO="bookworm"
ARG GOLANG_IMAGE="golang:${GO_VERSION}-${BASE_DEBIAN_DISTRO}"

FROM ${GOLANG_IMAGE} AS base

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

# Build the kernel
# Seperate from base to allow config construction from fragments in the future
FROM kernel-build-base AS kernel-build

#COPY --from=config-build /usr/src/fragments/.config /usr/src/linux/.config
COPY kernel/config-${KERNEL_VERSION}-${KERNEL_ARCH} /usr/src/linux/.config

# Compile the kernel
RUN cd linux && make -j${KERNEL_NPROC}

FROM base AS shim-build

WORKDIR /go/src/github.com/dmcgowan/nerdbox

ARG GO_DEBUG_GCFLAGS
ARG GO_GCFLAGS
ARG GO_BUILD_FLAGS
ARG TARGETPLATFORM

RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=shim-build-$TARGETPLATFORM \
    go build ${GO_DEBUG_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o /build/containerd-shim-nerdbox-v1 -ldflags '-s -w' -tags 'no_grpc' ./cmd/containerd-shim-nerdbox-v1

FROM base AS vminit-build

WORKDIR /go/src/github.com/dmcgowan/nerdbox

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

RUN mkdir /build && wget -O /build/crun https://github.com/containers/crun/releases/download/1.24/crun-1.24-linux-amd64-disable-systemd

FROM base AS initrd-build
WORKDIR /usr/src/init
ARG TARGETPLATFORM
RUN --mount=type=cache,sharing=locked,id=initrd-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=initrd-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends cpio

RUN mkdir sbin proc sys tmp run

COPY --from=vminit-build /build/vminitd ./init
COPY --from=crun-build /build/crun ./sbin/crun

RUN << EOT
    set -e
    chmod +x sbin/crun
    mkdir /build
    (find . -print0 | cpio --null -H newc -o ) | gzip -9 > /build/nerdbox-initrd
EOT

FROM scratch AS kernel
ARG KERNEL_ARCH="x86_64"
COPY --from=kernel-build /usr/src/linux/vmlinux /nerdbox-kernel-${KERNEL_ARCH}

FROM scratch AS initrd
COPY --from=initrd-build /build/nerdbox-initrd /nerdbox-initrd

FROM scratch AS shim
COPY --from=shim-build /build/containerd-shim-nerdbox-v1 /containerd-shim-nerdbox-v1
