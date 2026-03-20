variable "HOST_OS" {
  default = "linux"
}

variable "KERNEL_VERSION" {
  default = "6.12.44"
}

variable "KERNEL_ARCH" {
  default = "x86_64"
}

variable "KERNEL_NPROC" {
  default = "4"
}

variable "KERNEL_PAGE_SIZE" {
  default = "4k"
}

variable "GO_BUILD_FLAGS" {
  default = ""
}

variable "GO_GCFLAGS" {
  default = ""
}

variable "GO_DEBUG_GCFLAGS" {
  default = ""
}

variable "GO_LDFLAGS" {
  default = ""
}

variable "GOLANGCI_LINT_MULTIPLATFORM" {
  default = ""
}

variable "GOPROXY" {
  default = ""
}

# Map KERNEL_ARCH to Docker platform architecture names.
variable "_DOCKER_ARCH" {
  default = KERNEL_ARCH == "x86_64" ? "amd64" : KERNEL_ARCH == "aarch64" ? "arm64" : KERNEL_ARCH
}

target "_common" {
  args = {
    KERNEL_VERSION = KERNEL_VERSION
    KERNEL_ARCH = KERNEL_ARCH
    KERNEL_NPROC = KERNEL_NPROC
    KERNEL_PAGE_SIZE = KERNEL_PAGE_SIZE
    GO_BUILD_FLAGS = GO_BUILD_FLAGS
    GO_GCFLAGS = GO_GCFLAGS
    GO_DEBUG_GCFLAGS = GO_DEBUG_GCFLAGS
    GO_LDFLAGS = GO_LDFLAGS
    GOPROXY = GOPROXY
  }
}

target "_host_common" {
  inherits = ["_common"]
  platforms = ["${HOST_OS}/${_DOCKER_ARCH}"]
}

target "_guest_common" {
  inherits = ["_common"]
  args = {
    TARGETOS = "linux"
  }
}

variable "DESTDIR" {
  default = "_output"
}

group "default" {
  targets = ["host-binaries", "guest-binaries", "kernel"]
}

group "host-binaries" {
  targets = ["shim", "libkrun"]
}

group "guest-binaries" {
  targets = ["initrd"]
}

target "menuconfig" {
  inherits = ["_common"]
  target = "kernel-build-base"
  output = ["type=image,name=nerdbox-menuconfig"]
}

target "kernel" {
  inherits = ["_guest_common"]
  target = "kernel"
  output = ["${DESTDIR}"]
}

target "initrd" {
  inherits = ["_guest_common"]
  target = "initrd"
  output = ["${DESTDIR}"]
}

target "shim" {
  inherits = ["_host_common"]
  target = "shim"
  output = ["${DESTDIR}"]
}

target "libkrun" {
  inherits = ["_common"]
  target = "libkrun"
  platforms = ["linux/${_DOCKER_ARCH}"]
  output = ["${DESTDIR}"]
}

target "dev" {
  inherits = ["_common"]
  target = "dev"
  output = ["type=image,name=nerdbox-dev"]
}

group "validate" {
  targets = ["lint", "validate-dockerfile"]
}

target "lint" {
    name = "lint-${build.name}"
    inherits = ["_common"]
    output = ["type=cacheonly"]
    target = build.target
    args = {
        TARGETNAME = build.name
        GOLANGCI_FROM_SOURCE = "true"
    }
    platforms = (build.target == "golangci-lint") && (GOLANGCI_LINT_MULTIPLATFORM != null) ? [
        "linux/amd64",
        "linux/arm64",
        "darwin/amd64",
        "darwin/arm64",
        // "windows/amd64",
        // "windows/arm64",
    ] : []
    matrix = {
        build = [
            {
                name = "default",
                target = "golangci-lint",
            },
            {
                name = "golangci-verify",
                target = "golangci-verify",
            },
            {
                name = "yaml",
                target = "yamllint",
            },
        ]
    }
}

target "validate-dockerfile" {
    matrix = {
        dockerfile = [
            "Dockerfile",
        ]
    }
    name = "validate-dockerfile-${md5(dockerfile)}"
    inherits = ["_common"]
    dockerfile = dockerfile
    call = "check"
}
