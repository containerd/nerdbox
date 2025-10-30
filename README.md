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

> #### Mac OS Tip
> 
> On mac, try running with these commands:
> ```
> $ make KERNEL_ARCH=arm64 KERNEL_NPROC=12 KERNEL_VERSION=6.12.44
> $ make _output/containerd-shim-nerdbox-v1 _output/nerdbox-initrd
> ```

### Configuring containerd

For Linux, the default configuration should work. On Linux, a snapshot could be
mounted on the host and passed to the VM via virtio-fs. For Mac OS, the erofs
snapshotter is required. Currently, to run on Mac OS, this requires using a
beta version of containerd 2.2. Use containerd v2.2.0-beta.2 or later:
https://github.com/containerd/containerd/releases/tag/v2.2.0-beta.2

#### Enabling erofs in containerd config toml

If you don't have a containerd config file yet, generate one with:

```bash
$ containerd config default > config.toml
```

#### Update erofs differ

On mac, the mkfs.erofs tool might use a large block size which will get rejected
by the kernel running inside the VM. Ensure mkfs.erofs uses a 4k block size
by adding the mkfs option under the erofs differ.


```toml
  [plugins.'io.containerd.differ.v1.erofs']
    mkfs_options = ['-b4096']
```

#### Add unpack configuration option

The transfer service needs to be configured to use the erofs snapshotter for
unpacking linux/arm64 images.

```toml
  [plugins.'io.containerd.transfer.v1.local']
    #... ommitted

    [[plugins."io.containerd.transfer.v1.local".unpack_config]]
      platform = "linux/arm64"
      snapshotter = "erofs"
      differ = "erofs"
```

#### Add default size to snapshotter

```toml
  [plugins.'io.containerd.snapshotter.v1.erofs']
    default_size = "64M"

```

### Running

Install libkrun, erofs-utils, e2fsprogs on your host

> #### Mac OS Tip
>
> Brew install libkrun, erofs-utils, and e2fsprogs
> 
> ```
> brew tap slp/krunkit
> brew install libkrun-efi erofs-utils e2fsprogs
> ```

Run containerd with the shim and nerdbox components in the PATH:

```bash
$ PATH=$(pwd)/_output:$PATH containerd
```

> #### Mac OS Tip
>
> When running containerd, mkfs.ext4 may not be added to path by homebrew
>
> `PATH=$(pwd)/_output:/opt/homebrew/opt/e2fsprogs/sbin:$PATH containerd -c ./config.toml`
>

Pull a container down, select the platform and erofs snapshotter for Mac OS:

```bash
$ ctr image pull --platform linux/arm64 --snapshotter erofs docker.io/library/alpine:latest
```

Start a containerd with the nerdbox runtime (add snapshotter for Mac OS):

```bash
$ ctr run -t --rm --snapshotter erofs --runtime io.containerd.nerdbox.v1 docker.io/library/alpine:latest test /bin/sh
```

### Rootless on Mac OS

Root is not needed to run this on Mac OS, however, the configuration may need to
be updated.

By default, ensure `/var/lib/containerd` and `/var/run/containerd` are owned by
the user. Alternatively, the config can updated to reference any directories.
Update the containerd config toml file.

```toml
root = '/var/lib/containerd'
state = '/var/run/containerd'
```

Also ensure that the grpc socket is owned by the non root user.

```toml
[grpc]
  address = '/var/run/containerd/containerd.sock'
  uid = 501
  gid = 20
```
