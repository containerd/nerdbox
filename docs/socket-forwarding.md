# UNIX Socket Forwarding

Nerdbox supports forwarding UNIX domain sockets across the VM boundary,
allowing processes inside a container to connect to host-side services.
Sockets are relayed over vsock streams managed by vminitd.

## Configuration

Socket forwards are specified as OCI mounts with type `uds`. The shim
intercepts these mounts before the OCI runtime sees them.

| Mount field   | Description                                              |
|---------------|----------------------------------------------------------|
| `type`        | Must be `uds`                                            |
| `source`      | Path of the UNIX socket on the host (the target service) |
| `destination` | Path of the UNIX socket inside the container             |

A **forward identifier** (`forward_id`) is derived from
`SHA256(container_id + ":" + destination)` and serves as an opaque key
used in host-to-VM communication so the VM never needs to know (or supply)
host-side paths (see [Security model](#security-model)). Including the
container ID ensures that the same socket mount on two different containers
within the same VM gets a distinct identifier.

The VM listener socket is created at `/run/socketfwd/{forward_id}.sock`
(the `vm_path`). The shim rewrites each `uds` mount to a bind mount from
`vm_path` to `destination` before passing the spec to crun.

### Example: Forward a host socket into a container

Make the host's Docker socket available inside the container at
`/var/run/docker.sock`:

```json
{
  "type": "uds",
  "source": "/var/run/docker.sock",
  "destination": "/var/run/docker.sock"
}
```

## How It Works

Socket forwarding is coordinated between the shim (host side) and a vminitd
plugin (container side) using the `SocketForward` ttrpc service.

### Setup (Bind)

After the VM is started, but before the container is created, the shim calls
the `Bind` RPC with the list of socket forward entries. For each entry the
VM-side plugin creates a UNIX listener socket at `vm_path` (`/run/socketfwd/{forward_id}.sock`).
The shim has already rewritten the `uds` mount to a bind mount from `vm_path`
to `destination`, so once crun processes the spec the socket appears inside
the container at the user-specified `destination`.

`Bind` returns only after all socket files have been created, so the shim
can safely proceed with container creation.

### Forwarding flow (Accept)

1. The `Bind` RPC (see above) has already created a UNIX listener at
   `vm_path`, with a bind mount so the socket appears inside the container
   at `destination`.
2. When a container process connects, the vminitd plugin generates a unique
   `stream_id` and sends a `ConnectRequest` containing the `stream_id`
   and `forward_id` through the `Accept` streaming RPC.
3. The shim receives the notification and resolves `forward_id` to a
   host-side socket path using its own local configuration. It then dials
   that host socket and opens a vsock stream with the given `stream_id`.
4. The vminitd plugin matches the vsock stream to the pending connection and
   relays data bidirectionally.

```
Container process  ──► listener (/run/socketfwd/{forward_id}.sock)
                           │                   ▲
                      bind mount to destination
                           │
                     ConnectRequest
                     (stream_id + forward_id)
                     via Accept stream
                           │
                           ▼
                    shim receives notification
                    resolve forward_id ──► host_path
                           │
                    dial host_path ──► Host socket
                    open vsock stream
                           │
                           ▼
                    bidirectional relay
```

### Caveats

The container-side `connect` syscall always succeeds immediately (it connects
to the vminitd listener socket). If the shim subsequently fails to dial the
host socket — for example because the host service is not running — the
connection is closed immediately after the container-side `connect` returns.
From the container process's perspective this looks like the peer closed the
connection right away.

## Security model

The VM is treated as a security boundary. The shim (host side) never trusts
paths or other security-sensitive values supplied by the VM. Concretely:

- The `host_path` is **never sent to the VM**. The `Bind` RPC sends only
  the `forward_id` and `vm_path`.
- When the VM notifies the shim about a new connection (via `Accept`), it
  sends a `forward_id` -- not a host path. The shim resolves the identifier
  to a host path from its own configuration, which was derived from the OCI
  mounts at container creation time.
- A `ConnectRequest` with an unknown `forward_id` is rejected and logged.

This design ensures that even a compromised VM cannot cause the shim to
connect to arbitrary host sockets.
