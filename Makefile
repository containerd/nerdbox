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

GO ?= go
DOCKER ?= docker
BUILDX ?= $(DOCKER) buildx

ROOTDIR=$(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))

WHALE = "🇩"
ONI = "👹"

ARCH = $(shell uname -m)
OS = $(shell uname -s)
LDFLAGS_x86_64_Linux = -lkrun
LDFLAGS_aarch64_Linux = -lkrun
LDFLAGS_arm64_Darwin = -L/opt/homebrew/lib -lkrun
CFLAGS = -O2 -g -I../include

LDFLAGS += -s -w
DEBUG_GO_GCFLAGS :=
DEBUG_TAGS :=

ifdef BUILDTAGS
    GO_BUILDTAGS = ${BUILDTAGS}
endif
GO_BUILDTAGS ?= no_grpc
GO_BUILDTAGS += ${DEBUG_TAGS}

GO_STATIC_BUILDTAGS ?=
GO_STATIC_BUILDTAGS += ${DEBUG_TAGS}
GO_STATIC_BUILDTAGS += osusergo netgo static_build no_grpc

GO_TAGS=$(if $(GO_BUILDTAGS),-tags "$(strip $(GO_BUILDTAGS))",)
GO_STATIC_TAGS=$(if $(GO_STATIC_BUILDTAGS),-tags "$(strip $(GO_STATIC_BUILDTAGS))",)

GO_BUILD_FLAGS ?=
GO_LDFLAGS ?= -ldflags '$(LDFLAGS) $(EXTRA_LDFLAGS)'
GO_STATIC_LDFLAGS := -ldflags '-extldflags "-static" $(LDFLAGS) $(EXTRA_LDFLAGS)'

MODULE_NAME=$(shell go list -m)
API_PACKAGES=$(shell ($(GO) list ${GO_TAGS} ./... | grep /api/ ))

.PHONY: clean all validate lint generate protos check-protos check-api-descriptors proto-fmt shell

all:
	$(BUILDX) bake

_output/containerd-shim-nerdbox-v1: cmd/containerd-shim-nerdbox-v1 FORCE
	@echo "$(WHALE) $@"
	$(GO) build ${DEBUG_GO_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o $@ ${GO_LDFLAGS} ${GO_TAGS} ./$<
ifeq ($(OS),Darwin)
	codesign --entitlements cmd/containerd-shim-nerdbox-v1/containerd-shim-nerdbox-v1.entitlements --force -s - $@
endif

_output/vminitd: cmd/vminitd FORCE
	@echo "$(WHALE) $@"
	$(GO) build ${DEBUG_GO_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o $@ ${GO_STATIC_LDFLAGS} ${GO_STATIC_TAGS}  ./$<

_output/nerdbox-initrd: cmd/vminitd FORCE
	@echo "$(WHALE) $@"
	$(BUILDX) bake initrd

_output/test_vminitd: cmd/test_vminitd FORCE
	@echo "$(WHALE) $@"
	$(GO) build ${DEBUG_GO_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o $@ ${GO_LDFLAGS} ${GO_TAGS} ./$<
ifeq ($(OS),Darwin)
	codesign --entitlements cmd/test_vminitd/test_vminitd.entitlements --force -s - $@
endif

_output/run_vminitd: cmd/run_vminitd/main.c
	gcc -o $@ $< $(CFLAGS) $(LDFLAGS_$(ARCH)_$(OS))
ifeq ($(OS),Darwin)
	codesign --entitlements src/run_vminitd.entitlements --force -s - $@
endif

_output/libkrun.so: FORCE
	@echo "$(WHALE) $@"
	$(BUILDX) bake libkrun


generate: protos
	@echo "$(WHALE) $@"
	@PATH="${ROOTDIR}/bin:${PATH}" $(GO) generate -x ${PACKAGES}

protos:
	@echo "$(WHALE) $@"
	@(cd ${ROOTDIR}/api && PATH="${ROOTDIR}/bin:${PATH}" protobuild --quiet ${API_PACKAGES})
	go-fix-acronym -w -a '^Os' $(shell find api/ -name '*.pb.go')
	go-fix-acronym -w -a '(Id|Io|Uuid|Os)$$' $(shell find api/ -name '*.pb.go')

check-protos: protos ## check if protobufs needs to be generated again
	@echo "$(WHALE) $@"
	@test -z "$$(git status --short | grep ".pb.go" | tee /dev/stderr)" || \
		((git diff | cat) && \
		(echo "$(ONI) please run 'make protos' when making changes to proto files" && false))

check-api-descriptors: protos ## check that protobuf changes aren't present.
	@echo "$(WHALE) $@"
	@test -z "$$(git status --short | grep ".pb.txt" | tee /dev/stderr)" || \
		((git diff $$(find . -name '*.pb.txt') | cat) && \
		(echo "$(ONI) please run 'make protos' when making changes to proto files and check-in the generated descriptor file changes" && false))

proto-fmt: ## check format of proto files
	@echo "$(WHALE) $@"
	@test -z "$$(find . -name '*.proto' -type f -exec grep -Hn -e "^ " {} \; | tee /dev/stderr)" || \
		(echo "$(ONI) please indent proto files with tabs only" && false)

menuconfig:
ifeq ($(KERNEL_VERSION),)
	$(error KERNEL_VERSION is not set)
endif
ifeq ($(KERNEL_ARCH),)
	$(error KERNEL_ARCH is not set)
endif
	@echo "$(WHALE) $@"
	@$(BUILDX) bake menuconfig
	docker run --rm -it \
		-v ./kernel:/config \
		-w /usr/src/linux \
		-e KCONFIG_CONFIG=/config/config-$(KERNEL_VERSION)-$(KERNEL_ARCH) \
		nerdbox-menuconfig \
		make menuconfig

FORCE:

validate:
	@$(BUILDX) bake validate

lint:
	@$(BUILDX) bake lint

clean:
	rm -rf _output

shell:
	@echo "$(WHALE) $@"
	@$(BUILDX) bake dev
	@docker run --rm -it --privileged \
		--name nerdbox-dev \
		-v ./:/go/src/$(MODULE_NAME) \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-w /go/src/$(MODULE_NAME) \
		-e KERNEL_ARCH=$(ARCH) \
		-e KERNEL_NPROC \
		$(DOCKER_EXTRA_ARGS) \
		nerdbox-dev

verify-vendor: ## verify if all the go.mod/go.sum files are up-to-date
	@echo "$(WHALE) $@"
	$(eval TMPDIR := $(shell mktemp -d))
	@cp -R ${ROOTDIR} ${TMPDIR}
	@(cd ${TMPDIR}/nerdbox && ${GO} mod tidy)
	@(cd ${TMPDIR}/nerdbox && ${GO} mod verify)
	diff -r -u ${ROOTDIR} ${TMPDIR}/nerdbox
	@rm -rf ${TMPDIR}
