# Nerdbox (contaiNERD sandBOX)

__(Experimental)__ Nerdbox is a containerd runtime which isolates container
processes using a virtual machine. It is designed for running containers
cross platform and with enhanced security.

## Getting Started

Building requires Docker with buildx installed.

Run `make` to build the shim, kernel, and nerdbox image:

```bash
$ make
```

The results will be in the `_output` directory.


Run containerd with the shim and nerdbox components in the PATH:

```bash
# PATH=$(pwd)/_output:$PATH containerd
```

Start a containerd with the nerdbox runtime:

```bash
# ctr run -t --rm --runtime io.containerd.nerdbox.v1 docker.io/library/alpine:latest test /bin/sh
```
