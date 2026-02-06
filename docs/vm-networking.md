# VM Networking

## TSI

By default, nerdbox creates microVMs with no network interface set up. In this
case, VMs get connectivity through TSI (i.e. Transparent Socket Impersonation).
With this networking mode, ports bound inside the VM are transparently mapped
on the host, and the VM can reach sockets listening on the host, as well as the
outside world.

This networking mode relies on a series of kernel patches maintained by the
libkrun team, and is licensed under GPL 2.0 / 2.1. See [github.com/containers/libkrunfw](https://github.com/containers/libkrunfw).

These patches automatically translate socket syscalls made for socket family
AF_INET, and socket types SOCK_STREAM and SOCK_DGRAM, into AF_TSI. As such,
this networking mode doesn't support IPv6 connections, and ICMP protocol.

## External network providers

Network interfaces can be attached to the VM by specifying the OCI annotations
`io.containerd.nerdbox.network.*`. These annotations are CSV-encoded strings
that take the following fields:

- `socket` (required): Path to a UNIX socket serving this network endpoint
- `mode` (required): Either `unixgram` or `unixstream` depending on whether
  `socket` was open with `socket(AF_UNIX, SOCK_DGRAM, ...)` or
  `socket(AF_UNIX, SOCK_STREAM, ...)`.
  - Following network providers are using unixgram sockets: [containers/gvisor-tap-vsock](https://github.com/containers/gvisor-tap-vsock),
    and [nirs/vmnet-helper](https://github.com/nirs/vmnet-helper).
  - Following network providers are using unixstream sockets: [passt](https://passt.top/),
    and [lima-vm/socket_vmnet](https://github.com/lima-vm/socket_vmnet).
- `mac` (reqired): MAC address
- `dhcp` (optional, defaults to false): tells the VM to retrieve the IPv4 address
  of this interface through DHCPv4.
- `addr` (required): IP address with subnet mask (in CIDR notation). This field
  can be specified either zero times (when `dhcp` is specified), or at most
  twice (once per IP family).
- `features` (optional, defaults to 0): Bitwise-OR separated list of virtio-net
  features. Supported features:
  - `VIRTIO_NET_F_CSUM`: Device handles packets with partial checksum offload
  - `VIRTIO_NET_F_GUEST_CSUM`: Driver handles packets with partial checksum
  - `VIRTIO_NET_F_GUEST_TSO4`: Driver can receive TSOv4
  - `VIRTIO_NET_F_GUEST_TSO6`: Driver can receive TSOv6
  - `VIRTIO_NET_F_GUEST_UFO`: Driver can receive UFO
  - `VIRTIO_NET_F_HOST_TSO4`: Device can receive TSOv4
  - `VIRTIO_NET_F_HOST_TSO6`: Device can receive TSOv6
  - `VIRTIO_NET_F_HOST_UFO`: Device can receive UFO
- `vfkit` (optional, defaults to false): Indicate whether the VMM must send the
  VFKIT magic sequence after connecting to the `socket`. Accept any of `1, t, T,
  TRUE, true, True, 0, f, F, FALSE, false, False`. Any other value is invalid and
  will produce an error.
- `vnet_hdr` (optional, defaults to false): Indicate whether the VMM includes
  virtio-net headers along with Ethernet frames.

Note that the first network specified will be used as the default gateway.

#### gvisor-tap-vsock

Here's an example using [containers/gvisor-tap-vsock](https://github.com/containers/gvisor-tap-vsock).
You must build it from source following the instructions set in that repository.

```
(terminal 1) $ bin/gvproxy -debug -listen unix:///tmp/network.sock -listen-vfkit unixgram:///tmp/gvisor.sock
(terminal 2) $ ctr run -t --net-host --cap-add CAP_NET_ADMIN --rm --snapshotter erofs --runtime io.containerd.nerdbox.v1 \
  --annotation io.containerd.nerdbox.network.0=socket=/tmp/gvisor.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,addr=192.168.127.2 \
  docker.io/nicolaka/netshoot:latest test
```

#### nirs/vmnet-helper

This example uses [nirs/vmnet-helper](https://github.com/nirs/vmnet-helper), a
macOS-only network provider that leverages Apple's [`vmnet.framework`](https://developer.apple.com/documentation/vmnet).
It creates an interface on the host, and assign an host-reachable address to the
VM. The VM is configured through DHCP.

```
(terminal 1) $ sudo --non-interactive \
       /opt/vmnet-helper/bin/vmnet-helper \
       --socket /tmp/vmnet.sock \
       --interface-id 2835E074-9892-4A79-AFFB-7E41D2605678 2>/dev/null | jq -r .vmnet_mac_address
0a:d6:36:c1:ea:f3

(terminal 2) $ ctr run -t --net-host --cap-add CAP_NET_ADMIN --rm --snapshotter erofs --runtime io.containerd.nerdbox.v1 \
  --annotation io.containerd.nerdbox.network.0=socket=/tmp/vmnet.sock,mode=unixgram,mac=0a:d6:36:c1:ea:f3,dhcp=true \
  docker.io/nicolaka/netshoot:latest test
```
