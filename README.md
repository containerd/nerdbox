# nerdbox: containerd runtime shim with VM isolation

<picture>
  <source media="(prefers-color-scheme: light)" srcset="docs/images/nerdbox.svg">
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/nerdbox-white.svg">
  <img alt="logo" src="docs/images/nerdbox.svg">
</picture>

___(Experimental)___ nerdbox (contaiNERD sandBOX) is a containerd runtime shim which
isolates container processes using a virtual machine. It is designed for running
containers cross platform and with enhanced security.

 - Works with containerd running on native host
 - Runs Linux containers on Linux, macOS, and soon Windows
 - EROFS support on all platforms
 - Rootless by default
 - Allows one VM per container for maximum isolation
 - Multiple containers per VM for maximum efficiency ___coming soon___

nerdbox is a _prospective_ **non-core** sub-project of containerd.

## Getting Started

Building requires Docker with buildx installed.

Run `make` to build the shim, kernel, and nerdbox image:

```bash
$ make
```

The results will be in the `_output` directory.

> #### macOS Tip
> 
> On mac, try running with these commands:
> ```
> $ make KERNEL_ARCH=arm64 KERNEL_NPROC=12 KERNEL_VERSION=6.12.44
> $ make _output/containerd-shim-nerdbox-v1 _output/nerdbox-initrd
> ```

### Configuring containerd

For Linux, the default configuration should work. On Linux, a snapshot could be
mounted on the host and passed to the VM via virtio-fs. For macOS, the erofs
snapshotter is required. Currently, to run on macOS, this requires using a
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

> #### macOS Tip
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

> #### macOS Tip
>
> When running containerd, mkfs.ext4 may not be added to path by homebrew
>
> `PATH=$(pwd)/_output:/opt/homebrew/opt/e2fsprogs/sbin:$PATH containerd -c ./config.toml`
>

Pull a container down, select the platform and erofs snapshotter for macOS:

```bash
$ ctr image pull --platform linux/arm64 --snapshotter erofs docker.io/library/alpine:latest
```

Start a containerd with the nerdbox runtime (add snapshotter for macOS):

```bash
$ ctr run -t --rm --snapshotter erofs --runtime io.containerd.nerdbox.v1 docker.io/library/alpine:latest test /bin/sh
```

### Rootless on macOS

Root is not needed to run this on macOS, however, the containerd configuration
may need to be updated to run containerd as a non-root user.

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

## How does this compare with other projects?

### Runtimes in Linux virtual machines
 - **lima** runs containerd in Linux VMs to provide the containerd API from
   inside the VM to clients, such as nerdctl, running on the host.
 - **Docker Desktop** runs dockerd in a Linux VM on macOS and Windows,
   providing the docker API to docker CLIs running on the host.

nerdbox is similar in that it uses a VM for isolation, but nerdbox is designed
to be a containerd runtime shim with containerd running outside the VM directly
on the host.

### Low level container runtimes
 - **Kata Containers** is a project that provides a containerd runtime with VM
   isolation. It is a mature project with support for multiple hypervisors but
   limited to running on Linux hosts.
 - **gVisor** is a project that provides a containerd runtime with
   enhanced security using a user-space kernel. The user-space kernel is very
   lightweight but limits gVisor to only running on Linux hosts.
 - **Apple Containerization** runs Linux containers in a lightweight VM on macOS
   using Apple Virtualization framework. It is not supported as a containerd
   runtime and requires using its own tooling to manage containers instead.

nerdbox also uses a lightweight VM for maximum isolation, but nerdbox is
designed to run on any platform supported by containerd, including both macOS
and Linux. nerdbox also uses the latest features in containerd such as EROFS,
mount manager, and the sandbox shim API. Since nerdbox is cross platform by
design, it avoids both image filesystem operations and container process
management on the host, allowing a seamless and efficient rootless mode.
