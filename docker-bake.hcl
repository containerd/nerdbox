variable "KERNEL_VERSION" {
  default = "6.12.46"
}

variable "KERNEL_ARCH" {
  default = "x86_64"
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

target "_common" {
  args = {
    KERNEL_VERSION = KERNEL_VERSION
    KERNEL_ARCH = KERNEL_ARCH
    KERNEL_NPROC = KERNEL_NPROC
    GO_BUILD_FLAGS = GO_BUILD_FLAGS
    GO_GCFLAGS = GO_GCFLAGS
    GO_DEBUG_GCFLAGS = GO_DEBUG_GCFLAGS
    GO_LDFLAGS = GO_LDFLAGS
  }
}

variable "DESTDIR" {
  default = "_output"
}

target "menuconfig" {
  inherits = ["_common"]
  target = "kernel-build-base"
  output = ["type=image,name=nerdbox-menuconfig"]
}

target "kernel" {
  inherits = ["_common"]
  target = "kernel"
  output = ["${DESTDIR}"]
}

target "initrd" {
  inherits = ["_common"]
  target = "initrd"
  output = ["${DESTDIR}"]
}

target "shim" {
  inherits = ["_common"]
  target = "shim"
  output = ["${DESTDIR}"]
}

target "libkrun" {
  inherits = ["_common"]
  target = "libkrun"
  output = ["${DESTDIR}"]
}

group "default" {
    targets = ["kernel", "initrd", "shim"]
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
