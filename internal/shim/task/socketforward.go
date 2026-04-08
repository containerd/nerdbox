/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package task

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"

	socketforward "github.com/containerd/nerdbox/api/services/socketforward/v1"
	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// socketForwardEntry describes a single UDS socket forward. The host-side
// socket is the target service; the container-side path is the listener
// socket created by vminitd so that container processes can connect to it.
type socketForwardEntry struct {
	// id is an opaque identifier for this forward, derived from
	// SHA256(containerID + ":" + destination). It is passed to vminitd so
	// the VM can reference the forward without knowing the host-side path.
	// The container ID is included so that the same socket mount on two
	// different containers within the same VM gets a distinct identifier.
	id string
	// hostPath is the path of the UNIX socket on the host. This value is
	// only used by the shim (host side) and is never sent to the VM.
	hostPath string
	// vmPath is the path of the UNIX listener socket in the VM's root
	// filesystem, set to /run/socketfwd/{id}.sock. It is the source of the
	// OCI bind mount that makes the socket visible inside the container at
	// containerPath. vminitd creates the socket here before container
	// creation so that crun can complete the bind mount.
	vmPath string
	// containerPath is the user-specified destination: where the socket
	// appears inside the container rootfs.
	containerPath string
}

// socketForwardsProvider parses OCI mounts with type "uds" and generates the
// init args and sandbox options needed for socket forwarding.
type socketForwardsProvider struct {
	// containerID must be set before calling FromBundle.
	containerID string
	entries     []socketForwardEntry
}

// FromBundle rewrites UDS mounts (type "uds") in the OCI bundle spec to
// ordinary bind mounts that crun can process. Non-UDS mounts are left
// untouched.
//
// UDS mount fields:
//   - Source: host-side UNIX socket path (the target service)
//   - Destination: container-side UNIX socket path
//   - Options: none
//
// Each UDS mount is replaced in-place with a bind mount whose source is
// vmPath (/run/socketfwd/{id}.sock) and whose destination is the original
// container-side path. vminitd creates the listener socket at vmPath before
// container creation so that crun can complete the bind mount.
func (p *socketForwardsProvider) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	for i, m := range b.Spec.Mounts {
		if m.Type != "uds" {
			continue
		}

		entry, err := parseUDSMount(p.containerID, m)
		if err != nil {
			return fmt.Errorf("parsing uds mount for %q: %w", m.Destination, err)
		}

		b.Spec.Mounts[i] = specs.Mount{
			Destination: entry.containerPath,
			Type:        "bind",
			Source:      entry.vmPath,
			Options:     []string{"bind"},
		}

		log.G(ctx).WithFields(log.Fields{
			"id":          entry.id,
			"source":      entry.hostPath,
			"destination": entry.containerPath,
			"vm_path":     entry.vmPath,
		}).Debug("socketforward: added bind mount for UDS forward")

		p.entries = append(p.entries, entry)
	}
	return nil
}

func parseUDSMount(containerID string, m specs.Mount) (socketForwardEntry, error) {
	if m.Source == "" {
		return socketForwardEntry{}, fmt.Errorf("source (host path) is required")
	}
	if m.Destination == "" {
		return socketForwardEntry{}, fmt.Errorf("destination (container path) is required")
	}
	if len(m.Options) > 0 {
		return socketForwardEntry{}, fmt.Errorf("unknown option %q", strings.Join(m.Options, ", "))
	}

	hash := sha256.Sum256([]byte(containerID + ":" + m.Destination))
	return socketForwardEntry{
		id:            fmt.Sprintf("%x", hash),
		hostPath:      m.Source,
		vmPath:        fmt.Sprintf("/run/socketfwd/%x.sock", hash),
		containerPath: m.Destination,
	}, nil
}

// bindSockets calls the Bind RPC on the VM to set up socket forward
// listener sockets. This must be called before container creation so that
// crun can bind-mount the listener sockets into the container.
func bindSockets(ctx context.Context, sb sandbox.Sandbox, entries []socketForwardEntry) error {
	if len(entries) == 0 {
		return nil
	}

	vmc, err := sb.Client()
	if err != nil {
		return fmt.Errorf("getting ttrpc client for socket forwarding: %w", err)
	}

	sockets := make([]*socketforward.Socket, 0, len(entries))
	for _, e := range entries {
		sockets = append(sockets, &socketforward.Socket{
			ForwardID:  e.id,
			SocketPath: e.vmPath,
		})
	}

	sfClient := socketforward.NewTTRPCSocketForwardClient(vmc)
	if _, err := sfClient.Bind(ctx, &socketforward.BindRequest{
		Sockets: sockets,
	}); err != nil {
		return fmt.Errorf("bind RPC: %w", err)
	}
	return nil
}

// socketForwarder manages active UDS socket forwarding for a single container.
// It is started after the container is created and runs for the container
// lifetime.
type socketForwarder struct {
	sb          sandbox.Sandbox
	containerID string
	// entries maps a forward identifier to its entry.
	entries map[string]socketForwardEntry
	// closeCh is closed when the socket forwarder is shutting down.
	closeCh chan struct{}
}

// startSocketForwarding creates a socketForwarder and starts the Accept loop
// for forwarded connection notifications from the VM.
func startSocketForwarding(ctx context.Context, sb sandbox.Sandbox, containerID string, entries []socketForwardEntry) (*socketForwarder, error) {
	fwd := &socketForwarder{
		sb:          sb,
		containerID: containerID,
		entries:     make(map[string]socketForwardEntry, len(entries)),
		closeCh:     make(chan struct{}),
	}

	vmc, err := sb.Client()
	if err != nil {
		return nil, fmt.Errorf("getting ttrpc client for socket forwarding: %w", err)
	}
	sfClient := socketforward.NewTTRPCSocketForwardClient(vmc)

	for _, e := range entries {
		fwd.entries[e.id] = e
	}

	go fwd.acceptLoop(ctx, sb, sfClient)

	return fwd, nil
}

// acceptLoop connects to the Accept stream and dispatches forwarded
// connection notifications from the VM.
func (fwd *socketForwarder) acceptLoop(ctx context.Context, sb sandbox.Sandbox, sfClient socketforward.TTRPCSocketForwardClient) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := sfClient.Accept(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("socketforward: Accept RPC failed")
		return
	}

	results := make(chan *socketforward.ConnectResult, 64)
	go func() {
		for {
			select {
			case r, ok := <-results:
				if !ok {
					return
				}
				if err := stream.Send(r); err != nil {
					log.G(ctx).WithError(err).Error("socketforward: sending ConnectResult")
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
					log.G(ctx).WithError(err).Error("socketforward: Accept stream error")
				}
				return
			}
			select {
			case results <- fwd.handleConnection(ctx, sb, req):
			case <-fwd.closeCh:
				return
			}

		}
	}()

	<-fwd.closeCh
}

func (fwd *socketForwarder) handleConnection(ctx context.Context, sb sandbox.Sandbox, req *socketforward.ConnectRequest) *socketforward.ConnectResult {
	// Resolve the host path from local state. The VM only supplies the
	// forward identifier; the host never trusts a path from the VM.
	entry, ok := fwd.entries[req.ForwardID]
	if !ok {
		log.G(ctx).WithField("forward_id", req.ForwardID).Error("socketforward: unknown forward ID from VM")
		return &socketforward.ConnectResult{
			StreamID: req.StreamID,
			Error:    fmt.Sprintf("unknown forward ID: %s", req.ForwardID),
		}
	}

	log.G(ctx).WithFields(log.Fields{
		"stream_id":  req.StreamID,
		"forward_id": req.ForwardID,
		"host_path":  entry.hostPath,
	}).Debug("socketforward: new forwarded connection")

	// Dial the host-side target socket.
	hostConn, err := net.Dial("unix", entry.hostPath)
	if err != nil {
		log.G(ctx).WithError(err).WithField("host_path", entry.hostPath).Error("socketforward: failed to dial host socket")
		return &socketforward.ConnectResult{
			StreamID: req.StreamID,
			Error:    "failed to dial host socket",
		}
	}

	// Open a vsock stream so the VM side can associate it with the pending
	// connection and start relaying.
	vsockConn, err := sb.StartStream(ctx, req.StreamID)
	if err != nil {
		log.G(ctx).WithError(err).WithField("stream_id", req.StreamID).Error("socketforward: failed to open vsock stream")
		hostConn.Close()
		return &socketforward.ConnectResult{StreamID: req.StreamID, Error: err.Error()}
	}

	go relay(hostConn, vsockConn, fwd.closeCh)

	return &socketforward.ConnectResult{StreamID: req.StreamID}
}

// relay copies data bidirectionally between two connections until one side
// closes or errors. Both connections are closed when done.
func relay(a, b io.ReadWriteCloser, closeCh <-chan struct{}) {
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	go func() {
		<-closeCh
		a.Close()
		b.Close()
	}()

	<-done // one side finished
	a.Close()
	b.Close()
	<-done // wait for the other
}

func (fwd *socketForwarder) shutdown() error {
	if fwd == nil {
		return nil
	}
	select {
	case <-fwd.closeCh:
		// already closed
	default:
		close(fwd.closeCh)
	}
	return nil
}
