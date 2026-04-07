# Vsock Streaming

Nerdbox runs each container inside a lightweight VM. All communication
between the host (the containerd shim) and the guest (`vminitd`) crosses the
VM boundary over **virtio-vsock** channels that libkrun proxies through
host-side UNIX sockets.

Two vsock ports are registered at VM startup:

| Port | Host socket file | Purpose |
|------|-----------------|---------|
| 1025 | `<vmState>/run_vminitd.sock` | TTRPC control plane (Task, Bundle, etc.) |
| 1026 | `<vmState>/streaming.sock` | Raw bidirectional data streams |

This document describes the **streaming channel** (port 1026) -- the
infrastructure that carries container stdio and containerd proto streams
across the VM boundary.

## Transport

libkrun's `krun_add_vsock_port2` maps a guest vsock port to a host UNIX
socket file. The guest sees a standard `AF_VSOCK` listener; the host sees a
plain `AF_UNIX` socket. The guest CID is always 3.

Registration happens in the `Start` method of `internal/vm/libkrun/instance.go`:

```go
v.vmc.AddVSockPort(1025, socketPath)   // RPC
v.vmc.AddVSockPort(1026, v.streamPath) // Streams
```

On the guest side, the streaming plugin listens with `AF_VSOCK`
(`plugins/vminit/streaming/plugin.go`):

```go
l, err := vsock.ListenContextID(config.ContextID, config.Port, &vsock.Config{})
```

Each logical stream is a **separate connection** to the same host UNIX socket
(and thus the same vsock port). There is no in-band multiplexing -- each
`net.Dial` to `streaming.sock` produces an independent bidirectional pipe.

## Stream establishment protocol

A stream is identified by a **stream ID** -- an arbitrary string that both
sides agree on through an out-of-band channel (typically a TTRPC RPC, see
[Coordination pattern](#coordination-pattern) below).

### Wire-level handshake

All integers are big-endian. Strings are length-prefixed with a `uint32`.

```
Host                                   Guest (streaming plugin)
----                                   -----------------------
net.Dial("unix", streaming.sock)
                                       l.Accept() -> conn

write uint32(len(streamID))
write []byte(streamID)
                                       read uint32 -> idLen
                                       read idLen bytes -> streamID

                                       if duplicate stream ID:
                                         write uint32(len(errMsg))
                                         write []byte(errMsg)
                                         close(conn)
                                       else:
                                         streams[streamID] = conn
                                         write uint32(len(streamID))
                                         write []byte(streamID)   <- ack

read uint32 -> ackLen
read ackLen bytes -> ack
assert ack == streamID

-> net.Conn ready for raw I/O          -> conn ready for raw I/O
```

After the handshake completes, the connection carries **raw bytes** in both
directions. No further framing is applied at this layer (individual stream
users may add their own framing on top -- see [Proto streams](#containerd-proto-streams)
below).

If the guest rejects the stream (e.g. duplicate ID), it writes back an error
string instead of echoing the stream ID. The host detects the mismatch and
returns an error.

### Source references

- Host handshake: `vmInstance.StartStream` in `internal/vm/libkrun/instance.go`
- Guest accept loop: `service.Run` in `plugins/vminit/streaming/plugin.go`
- Length-prefixed write helper: `writeString` in `plugins/vminit/streaming/plugin.go`

## APIs

### Host side: `Sandbox.StartStream`

Defined in `internal/shim/sandbox/sandbox.go`:

```go
type Sandbox interface {
    // ...
    StartStream(context.Context, string) (net.Conn, error)
}
```

`StartStream` takes a stream ID, performs the handshake described above, and
returns a `net.Conn` that is connected to the corresponding vsock connection
inside the VM. The implementation lives in
`internal/vm/libkrun/instance.go`.

If the streaming socket file does not exist yet (the VM is still booting),
`StartStream` retries with exponential backoff for up to one second before
returning `ErrUnavailable`.

### Guest side: `stream.Manager`

Defined in `internal/vminit/stream/stream.go`:

```go
type Manager interface {
    Get(id string) (io.ReadWriteCloser, error)
}
```

`Get` returns the raw vsock connection for a given stream ID and **removes it
from the internal map**. This means each stream ID can only be claimed once --
the caller receives exclusive ownership and is responsible for closing the
connection. Calling `Get` with an unknown or already-claimed ID returns
`ErrNotFound`.

The implementation is the `Get` method in `plugins/vminit/streaming/plugin.go`.

### Guest side: `streaming.StreamGetter`

For proto-framed streams (see [Containerd proto streams](#containerd-proto-streams)
below), the streaming plugin also exposes a `StreamGetter`
(`plugins/vminit/streaming/plugin.go`) that wraps the raw connection
in a `vsockStream` providing `Send(typeurl.Any)` / `Recv() typeurl.Any`
with length-prefixed protobuf framing.

## Coordination pattern

The host and guest must agree on a stream ID before either side can use
the connection. Since the stream channel itself has no signaling mechanism,
the ID is always exchanged over the **TTRPC control channel** (port 1025).

The host generates a stream ID, opens the stream via `StartStream`, and then
sends the ID to the guest through a TTRPC request. The guest calls
`streams.Get(id)` to claim the connection.

```
Host                                Guest
----                                -----
streamID = generate()
conn = StartStream(streamID)
                                    (stream registered by accept loop)
RPC(streamID) ----TTRPC--->         receive RPC
                                    conn = streams.Get(streamID)
<-- bidirectional I/O -->
```

Currently all stream users follow this pattern -- stream IDs are embedded
in TTRPC request fields (e.g. `stream://` URIs for container stdio).

## Existing stream users

### Container stdio

When containerd sends a `Create` or `Exec` request with FIFO/pipe-based
stdio URIs, the shim opens one vsock stream per stdio file descriptor
(stdin, stdout, stderr) and rewrites the URIs to `stream://<id>`.

Host side (`createStreams` in `internal/shim/task/io.go`):

```go
sid := generateStreamID(idPrefix + "-stdin")
conn, _ := ss.StartStream(ctx, sid)
io.Stdin = fmt.Sprintf("stream://%s", sid)
```

The shim then runs `io.Copy` goroutines between the host-side FIFOs and the
vsock connections (`internal/shim/task/io_copystreams_unix.go`).

Guest side (`internal/vminit/runc/platform.go`): `vminitd` parses the
`stream://` URIs, calls `streams.Get(id)` to obtain the vsock connection,
and wires it into the container process's stdin/stdout/stderr (or PTY).

### Containerd proto streams

The shim exposes a containerd `Streaming` TTRPC service
(`plugins/shim/streaming/plugin.go`) that bridges containerd's
generic streaming API to the vsock channel. This is used by operations like
image transfer (`Transfer` RPC) that send structured protobuf messages
rather than raw bytes.

Unlike the other users, proto streams add their own framing on top of the
raw vsock connection:

```
Each frame = [uint32 big-endian length][serialized google.protobuf.Any]
```

The host bridge reads `typeurl.Any` messages from the TTRPC stream and
writes them as length-prefixed frames to the VM (`bridgeTTRPCToVM`), and
vice versa (`bridgeVMToTTRPC`). Both functions live in
`plugins/shim/streaming/plugin.go`.

Inside the VM, the `vsockStream` wrapper
(`plugins/vminit/streaming/plugin.go`) implements
`Send` / `Recv` with the same framing, providing a `streaming.Stream`
interface to guest-side plugins.

## Adding a new stream type

To add a new feature that relays data through vsock streams:

1. **Pick a stream ID prefix.** Use a descriptive prefix unique to your
   feature (e.g. `socketfwd-`, `myfeature-`) followed by a UUID or
   timestamp+random suffix. See `generateStreamID` in
   `internal/shim/task/io.go` for a reference pattern to avoid
   collisions.

2. **Open the stream.**
   - Host: call `sandbox.StartStream(ctx, streamID)` to get a `net.Conn`.
   - Guest: call `streams.Get(streamID)` to get an `io.ReadWriteCloser`.

3. **Relay data.** For raw byte streams, a simple bidirectional `io.Copy`
   loop is typical:

    ```go
    done := make(chan struct{}, 2)
    cp := func(dst io.Writer, src io.Reader) {
        io.Copy(dst, src)
        done <- struct{}{}
    }
    go cp(a, b)
    go cp(b, a)
    <-done
    a.Close()
    b.Close()
    <-done
    ```

    For structured messages, wrap the connection with length-prefixed proto
    framing (see [Containerd proto streams](#containerd-proto-streams)).

4. **Handle lifetime.** The stream is alive as long as both ends keep their
   connection open. When either side closes, `io.Copy` returns and the relay
   goroutines exit. Make sure both sides close their end to avoid leaked
   connections.

## Security considerations

The VM is treated as a security boundary. The shim (host side) must never
trust values supplied by the guest to make security-sensitive decisions.

- **Never expose host paths to the VM.** Host-side filesystem paths must
  never cross the vsock channel. If the guest needs to refer to a host-side
  resource, use an opaque identifier that the host resolves locally. This
  ensures that even a compromised VM cannot instruct the shim to access
  arbitrary host files or sockets.

- **Stream IDs are coordination tokens, not secrets.** They are transmitted
  in the clear over TTRPC and the vsock handshake. Any code inside the VM
  can call `streams.Get(id)` on any ID it knows -- do not rely on stream ID
  secrecy for access control.

- **No authentication on the stream channel.** The streaming plugin accepts
  any connection on its vsock port and registers it by whatever stream ID
  the caller provides. Access control decisions must be made before a stream
  is opened, typically when parsing configuration on the host side.

- **Validate guest-supplied identifiers on the host.** When the guest sends
  an identifier through a TTRPC request, the host must validate it against
  its own configuration before acting on it. Unknown or unexpected
  identifiers should be rejected and logged.

- **Protect the host-side socket file.** The UNIX socket file
  `<vmState>/streaming.sock` is the host entry point to the stream channel.
  Any host process that can connect to it can register arbitrary streams
  inside the VM. File permissions on the socket and the `<vmState>`
  directory should restrict access to the shim process.
