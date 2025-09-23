# Nerdbox (contaiNERD sandBOX)

__(Experimental)__ Nerdbox is a containerd runtime which isolates container
processes using a virtual machine. It is designed for running containers
cross platform and with enhanced security.

## Getting Started

### Building

Building requires Docker with buildx installed


Run `make` to build the shim, kernel, and nerdbox image:

```bash
$ make
```

The results will be in the `_output` directory.

> #### Mac OS Tip
> 
> On mac, try running with these commands:
> ```
> $ make KERNEL_ARCH=arm64 KERNEL_NPROC=12 KERNEL_VERSION=6.12.42
> $ make _output/containerd-shim-nerdbox-v1 _output/nerdbox-initrd
> ```

### Running

Install libkrun, erofs-utils, e2fsprogs on your host

> #### Mac OS Tip
>
> Brew install libkrun, erofs-utils, and e2fsprogs
> 
> `brew install libkrun-efi erofs-utils e2fsprogs`
>

Run containerd with the shim and nerdbox components in the PATH:

```bash
# PATH=$(pwd)/_output:$PATH containerd
```

> #### Mac OS Tip
>
> Build containerd with the [erofs darwin changes](https://github.com/dmcgowan/containerd/tree/v2.2.0-beta.0-erofs-darwin.0)
>
> Also, when running containerd, mkfs.ext4 may not be added to path by homebrew
>
> `PATH=$(pwd)/_output:/opt/homebrew/opt/e2fsprogs/sbin:$PATH containerd`
>

Start a containerd with the nerdbox runtime:

```bash
# ctr run -t --rm --runtime io.containerd.nerdbox.v1 docker.io/library/alpine:latest test /bin/sh
```

