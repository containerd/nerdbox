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

nerdbox is a **non-core** sub-project of containerd.

## Getting Started

Building requires Docker with buildx installed.

Run `make` to build the shim, kernel, and nerdbox image:

```bash
make
```

The results will be in the `_output` directory.

> #### macOS Tip
>
> On macOS, use these commands:
> ```bash
> make KERNEL_ARCH=arm64 KERNEL_NPROC=12
> make _output/containerd-shim-nerdbox-v1 _output/nerdbox-initrd
> ```

### Configuring containerd

For Linux, the default configuration should work. On Linux, a snapshot could be
mounted on the host and passed to the VM via virtio-fs. For macOS, the erofs
snapshotter is required. Currently, to run on macOS, this requires using
containerd 2.2 or later:
https://github.com/containerd/containerd/releases

See [`./examples/macos/config.toml`](./examples/macos/config.toml) for
how to configure containerd on macOS.

<details>
<summary>Manual configuration</summary>

#### Enabling erofs in containerd config toml

If you don't have a containerd config file yet, generate one with:

```bash
containerd config default > config.toml
```

#### Update erofs differ

On macOS, the mkfs.erofs tool may use a large block size which will get rejected
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
    # ... omitted

    [[plugins."io.containerd.transfer.v1.local".unpack_config]]
      platform = "linux/arm64"
      snapshotter = "erofs"
      differ = "erofs"
```

#### Add differ options

nerdctl needs the following configuration, as it does not use the transfer service yet.

<!-- https://github.com/containerd/nerdctl/issues/4570#issuecomment-3474216920 -->

```toml
  [plugins.'io.containerd.service.v1.diff-service']
    default = ['erofs', 'walking']
    sync_fs = false
```

#### Add default size to snapshotter

```toml
  [plugins.'io.containerd.snapshotter.v1.erofs']
    default_size = "64M"
```

</details>

### Running

Install libkrun, erofs-utils, e2fsprogs on your host

> #### macOS Tip
>
> Use brew to install libkrun, erofs-utils, and e2fsprogs
>
> ```
> brew tap slp/krun
> brew install libkrun erofs-utils e2fsprogs
> ```
>
> `libkrun-efi` fails to load with the 1.16.0 release. Both `libkrun` and `libkrun-efi`
> may be installed at the same time, but you may need to run `brew link libkrun`.

Run containerd with the shim and nerdbox components in the PATH:

```bash
PATH=$(pwd)/_output:$PATH containerd
```

> #### macOS Tip
>
> When running containerd, mkfs.ext4 may not be added to path by homebrew
>
> `PATH=$(pwd)/_output:/opt/homebrew/opt/e2fsprogs/sbin:$PATH containerd -c ./config.toml`
>

Pull a container down, select the platform and erofs snapshotter for macOS:

```bash
ctr image pull --platform linux/arm64 --snapshotter erofs docker.io/library/alpine:latest
```

Start a container with the nerdbox runtime (add snapshotter for macOS):

```bash
ctr run -t --rm --snapshotter erofs --runtime io.containerd.nerdbox.v1 docker.io/library/alpine:latest test /bin/sh
```

> #### macOS Tip
>
> Both erofs and nerdbox are defaults on macOS in containerd 2.2. They can be omitted
> from the command line but are included here for consistency with Linux.
>

### Rootless on macOS

Root is not needed to run this on macOS, however, the containerd configuration
may need to be updated to run containerd as a non-root user.

By default, ensure `/var/lib/containerd` and `/var/run/containerd` are owned by
the user. Alternatively, the config can be updated to reference directories
writable by the user, but updating the config to use user-writable directories is
currently not functional due to the issue [containerd#12444](https://github.com/containerd/containerd/issues/12444).

Also ensure that the grpc socket is owned by the non root user.

```toml
[grpc]
  address = '/var/run/containerd/containerd.sock'
  uid = 501
  gid = 20
```

## How does this compare with other projects?

### Runtimes in Linux virtual machines
 - **Lima** runs containerd in Linux VMs to provide the containerd API from
   inside the VM to clients, such as nerdctl. Lima supports non-containerd
   workloads too.
 - **Docker Desktop** runs dockerd in a Linux VM on macOS and Windows,
   providing the docker API to docker CLIs running on the host.

nerdbox is similar in that it uses a VM for isolation, but nerdbox is designed
to be a containerd runtime shim with containerd running outside the VM directly
on the host.

Also, while those projects only use a single VM for all the containers, nerdbox
allows allocating a dedicated VM for each container.

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

## Acknowledgements

- [**libkrun**](https://github.com/containers/libkrun) - A fast and efficient VMM written in Rust. Thanks to the entire Rust VMM ecosystem for making this possible.
- [**EROFS**](https://erofs.docs.kernel.org/) - A modern and efficient read-only filesystem in the Linux kernel with a great set of tools for making it easy to use with container images.
- [**containerd**](https://github.com/containerd/containerd) - Built an amazing ecosystem of container runtimes, interfaces, and tools to make it easy to integrate new innovations in container technology.
- [**docker**](https://docker.com) - Original developers of containerd and nerdbox with continuous support of the open source container ecosystem.
