# The host and guest OS may differ; set the host OS here.
variable "HOST_OS" {
  default = "linux"
}

variable "ARCH" {
  default = "amd64"
}

variable "KERNEL_VERSION" {
  default = "6.12.46"
}

variable "KERNEL_NPROC" {
  default = "4"
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

function "kernelArch" {
  params = []
  result = ARCH == "amd64" ? "x86_64" : (ARCH == "arm64" ? "arm64" : "unknown")
}

target "_common" {
  args = {
    TARGETARCH = ARCH
    KERNEL_VERSION = KERNEL_VERSION
    KERNEL_ARCH = kernelArch()
    KERNEL_NPROC = KERNEL_NPROC
    GO_BUILD_FLAGS = GO_BUILD_FLAGS
    GO_GCFLAGS = GO_GCFLAGS
    GO_DEBUG_GCFLAGS = GO_DEBUG_GCFLAGS
    GO_LDFLAGS = GO_LDFLAGS
  }
}

target "_host_common" {
  inherits = ["_common"]
  args = {
    TARGETOS = HOST_OS
  }
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
  targets = ["shim"]
}

group "guest-binaries" {
  targets = ["initrd", "libkrun"]
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
  inherits = ["_guest_common"]
  target = "libkrun"
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
