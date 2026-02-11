# Container Networking

## Connecting to VM networks

Containers can be connected to networks the VM that have been defined via the
`io.containerd.nerdbox.network.*` annotation, see [VM Configuration](vm-configuration.md#networking).

Each `io.containerd.nerdbox.ctr.network` annotation describes a single network
connection.

- `vmmac` (required): MAC address of the VM's interface, identifies the network.
- `mac` (optional): A MAC address to assign to the container's network interface.
- `addr` (optional, can be repeated): IP address with subnet mask (CIDR notation)
  for the container's interface.
- `ifname` (optional, default "eth<N>"): Name the container's network interface.
  Use `%d` for `<N>` to have the kernel assign a number, for example `eth%d`.
- `gw` (optional, can be repeated for IPv4/IPv6): Default gateways for the container.

For example:

```
--annotation io.containerd.nerdbox.ctr.network.0=vmmac=fa:43:25:5d:6f:b4,addr=192.168.127.111/24,ifname=mynet0,gw=192.168.127.1
```

## DNS configuration in containers

When the bundle does not include a mount with destination `/etc/resolv.conf`,
the content of the container's `resolv.conf` file can be supplied in annotation
`io.containerd.nerdbox.ctr.dns`.

When this annotation is not used, if VM networks are defined (so, not using TSI),
the container will use the VM's `/etc/resolv.conf` file. If there are no VM
networks, the container will use a copy of the host's `/etc/resolv.conf` file
if it has one.

The annotation's value is in CSV format, with each field becoming a line in the
`resolv.conf` file. For example:
```
--annotation io.containerd.nerdbox.ctr.dns=nameserver=1.1.1.1,nameserver=8.8.8.8
```
