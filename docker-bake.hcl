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
