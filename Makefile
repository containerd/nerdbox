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

ifeq (,$(shell command -v task 2>/dev/null))
$(error 'task' is required to build nerdbox. Install from https://taskfile.dev)
endif

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

.PHONY: clean all build validate lint generate protos check-protos check-api-descriptors proto-fmt shell

all: build

build:
	@task build

_output/containerd-shim-nerdbox-v1: cmd/containerd-shim-nerdbox-v1 FORCE
	@task build:shim

_output/containerd-shim-nerdbox-v1.exe: cmd/containerd-shim-nerdbox-v1 FORCE
	@echo "$(WHALE) $@"
	GOOS=windows GOARCH=amd64 $(GO) build ${GO_BUILD_FLAGS} -o $@ ${GO_LDFLAGS} ${GO_TAGS} ./$<

_output/vminitd: cmd/vminitd FORCE
	@echo "$(WHALE) $@"
	$(GO) build ${DEBUG_GO_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o $@ ${GO_STATIC_LDFLAGS} ${GO_STATIC_TAGS}  ./$<

_output/nerdbox-initrd: cmd/vminitd FORCE
	@task build:initrd

_output/integration.test: integration FORCE
	@task build:integration

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
	@task generate

protos:
	@task protos

check-protos: protos ## check if protobufs needs to be generated again
	@task check-protos

check-api-descriptors: protos ## check that protobuf changes aren't present.
	@task check-api-descriptors

proto-fmt: ## check format of proto files
	@task proto-fmt

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
	@task validate

lint:
	@task lint

clean:
	@task clean

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
	@task verify-vendor

test-unit:
	@task test:unit

test-integration: _output/integration.test
	@task test:integration
