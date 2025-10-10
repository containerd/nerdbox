# Container Networking

## Connecting to VM networks

Containers can be connected to networks the VM that have been defined via the
`io.containerd.nerdbox.network.*` annotation, see [vm-networking](vm-networking.md).

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
