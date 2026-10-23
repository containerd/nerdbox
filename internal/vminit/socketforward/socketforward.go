//go:build linux

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

package socketforward

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/emptypb"

	socketforward "github.com/containerd/nerdbox/api/services/socketforward/v1"
	"github.com/containerd/nerdbox/internal/vminit/stream"
)

// GenerateStreamID creates a pseudo-random stream ID with the given prefix.
// The format is "{prefix}-{nanosecond}-{random}" to minimize collisions.
func GenerateStreamID(prefix string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating stream ID: %w", err)
	}
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), base64.RawURLEncoding.EncodeToString(b[:])), nil
}

// Service implements the VM-side socket forwarding ttrpc service.
// The VM creates UNIX listener sockets inside the VM and notifies the host
// when a container process connects, so the host can open a vsock stream
// and relay data to the target host-side socket.
type Service struct {
	streams stream.Manager

	mu        sync.Mutex
	listeners []net.Listener
	// pending maps stream_id to a channel that receives the host's dial
	// result (nil on success, non-nil on failure) for each in-flight
	// connection. Entries are added by handleConnection before sending the
	// ConnectRequest and removed when the ConnectResult arrives or the
	// context is cancelled.
	pending map[string]chan error

	// notify delivers ConnectRequest messages to the Accept stream so the
	// host shim can set up the vsock relay for each new connection.
	notify chan *socketforward.ConnectRequest
}

// NewService creates a new socket forwarding service.
func NewService(streams stream.Manager) *Service {
	return &Service{
		streams: streams,
		pending: make(map[string]chan error),
		notify:  make(chan *socketforward.ConnectRequest, 64),
	}
}

// RegisterTTRPC registers the socket forwarding service with the ttrpc server.
func (s *Service) RegisterTTRPC(server *ttrpc.Server) error {
	socketforward.RegisterTTRPCSocketForwardService(server, s)
	return nil
}

// Bind sets up socket forward entries on the VM side. For each entry it
// creates a UNIX listener socket at the given socket_path. This method
// returns only after all socket files have been created.
func (s *Service) Bind(ctx context.Context, req *socketforward.BindRequest) (*emptypb.Empty, error) {
	return s.bindAll(ctx, req)
}

func (s *Service) bindAll(ctx context.Context, req *socketforward.BindRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Never unlink a live listener, even if the duplicate appears later in
	// this request. Preflight before creating anything so a duplicate cannot
	// tear down another container's forward during rollback.
	paths := make(map[string]struct{}, len(s.listeners)+len(req.Sockets))
	for _, l := range s.listeners {
		path, err := canonicalSocketPath(l.Addr().String())
		if err != nil {
			return nil, fmt.Errorf("resolving bound socket path: %w", err)
		}
		paths[path] = struct{}{}
	}
	for _, sock := range req.Sockets {
		if sock == nil {
			return nil, fmt.Errorf("nil socket forward entry")
		}
		// Cleaning a path containing ".." before following a symlink can
		// change which socket it names. Reject lexical aliases rather than
		// trying to infer their target from the cleaned path.
		if filepath.Clean(sock.SocketPath) != sock.SocketPath {
			return nil, fmt.Errorf("non-canonical socket forward path %q", sock.SocketPath)
		}
		// Linux chown interprets UINT32_MAX as "leave unchanged". Reject it
		// before binding any socket so the permission-aware RPC cannot report
		// success without setting the requested owner.
		if sock.Mode != 0 && sock.Mode != 0600 {
			return nil, fmt.Errorf("unsupported guest socket mode %#o for %s", sock.Mode, sock.SocketPath)
		}
		if sock.Mode != 0 && (sock.Uid == ^uint32(0) || sock.Gid == ^uint32(0)) {
			return nil, fmt.Errorf("reserved guest socket UID or GID for %s", sock.SocketPath)
		}
		path, err := canonicalSocketPath(sock.SocketPath)
		if err != nil {
			return nil, fmt.Errorf("resolving socket path %s: %w", sock.SocketPath, err)
		}
		if _, ok := paths[path]; ok {
			return nil, fmt.Errorf("socket forward listener already bound at %s", sock.SocketPath)
		}
		paths[path] = struct{}{}
	}

	initial := len(s.listeners)
	for _, sock := range req.Sockets {
		if err := s.bind(ctx, sock); err != nil {
			for _, l := range s.listeners[initial:] {
				_ = l.Close()
			}
			s.listeners = s.listeners[:initial]
			return nil, fmt.Errorf("binding socket forward listener at %s: %w", sock.SocketPath, err)
		}
	}

	return &emptypb.Empty{}, nil
}

// canonicalSocketPath resolves aliases in the parent directory without
// following the socket itself. Some parents may not exist until bind creates
// them, so resolve the nearest existing ancestor and append the missing tail.
func canonicalSocketPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(abs)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Join(resolved, filepath.Base(abs)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", err
		}
		missing = append(missing, filepath.Base(dir))
		dir = parent
	}
}

// Accept is a bidirectional streaming RPC used to coordinate forwarded
// connections. The VM sends a ConnectRequest when a container process
// connects to a forwarded socket; the host resolves the forward_id,
// dials the target host socket, opens a vsock stream, and sends back a
// ConnectResult reporting success or failure. On failure the VM closes
// the pending container connection immediately.
func (s *Service) Accept(ctx context.Context, srv socketforward.TTRPCSocketForward_AcceptServer) error {
	log.G(ctx).Debug("socketforward: Accept started")

	// Receive ConnectResult messages from the host and dispatch them to the
	// goroutines waiting in handleConnection.
	go func() {
		for {
			result, err := srv.Recv()
			if err != nil {
				return
			}
			s.mu.Lock()
			ch, ok := s.pending[result.StreamID]
			if ok {
				delete(s.pending, result.StreamID)
			}
			s.mu.Unlock()
			if !ok {
				log.G(ctx).WithField("stream_id", result.StreamID).Warn("socketforward: ConnectResult for unknown stream, ignoring")
				continue
			}
			var dialErr error
			if result.Error != "" {
				dialErr = errors.New(result.Error)
			}
			ch <- dialErr
		}
	}()

	for {
		select {
		case req := <-s.notify:
			if err := srv.Send(req); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// bind creates a UNIX listener at socketPath. When a connection arrives,
// it generates a stream ID, sends a ConnectRequest on the notify channel
// (for the Accept stream), then waits for the host to report the dial
// result via a ConnectResult before proceeding with the vsock relay.
func (s *Service) bind(ctx context.Context, sock *socketforward.Socket) error {
	forwardID, socketPath := sock.ForwardID, sock.SocketPath
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return fmt.Errorf("creating parent directory for %s: %w", socketPath, err)
	}

	var l net.Listener
	var err error
	if sock.Mode == 0600 {
		l, err = listenPrivate(socketPath, sock.Uid, sock.Gid)
	} else {
		// Keep legacy socket behavior unchanged when no ownership is requested.
		if err = os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale socket file %s: %w", socketPath, err)
		}
		l, err = net.Listen("unix", socketPath)
	}
	if err != nil {
		return err
	}

	s.listeners = append(s.listeners, l)

	log.L.WithFields(log.Fields{
		"forward_id":  forwardID,
		"socket_path": socketPath,
	}).Info("socketforward: listening for forwarded connections")

	go s.acceptLoop(context.Background(), l, forwardID)
	return nil
}

// listenPrivate prepares the socket behind a root-owned, 0700 staging
// directory. No unprivileged guest process can connect before its owner and
// 0600 mode are set, even if the VM process has a permissive umask.
func listenPrivate(socketPath string, uid, gid uint32) (net.Listener, error) {
	stagingDir, err := os.MkdirTemp(filepath.Dir(socketPath), ".socketfwd-")
	if err != nil {
		return nil, fmt.Errorf("creating private socket staging directory: %w", err)
	}
	defer os.Remove(stagingDir) // empty after publication or closing on failure
	stagedPath := filepath.Join(stagingDir, "socket")
	l, err := net.Listen("unix", stagedPath)
	if err != nil {
		return nil, fmt.Errorf("listening on private socket: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = l.Close() // unlinks the unpublished socket
		}
	}()
	if err := os.Chown(stagedPath, int(uid), int(gid)); err != nil {
		return nil, fmt.Errorf("setting socket owner: %w", err)
	}
	if err := os.Chmod(stagedPath, 0600); err != nil {
		return nil, fmt.Errorf("setting socket mode: %w", err)
	}
	identity, err := os.Stat(stagedPath)
	if err != nil {
		return nil, fmt.Errorf("stat private socket: %w", err)
	}
	if err := os.Rename(stagedPath, socketPath); err != nil {
		return nil, fmt.Errorf("publishing private socket: %w", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	published = true
	return &privateListener{Listener: l, path: socketPath, identity: identity}, nil
}

// privateListener owns the published path: net.UnixListener only knows the
// staging path and must not leave the renamed socket behind on Close.
type privateListener struct {
	net.Listener
	path     string
	identity os.FileInfo
}

func (l *privateListener) Addr() net.Addr {
	return &net.UnixAddr{Name: l.path, Net: "unix"}
}

func (l *privateListener) Close() error {
	closeErr := l.Listener.Close()
	info, err := os.Stat(l.path)
	if os.IsNotExist(err) {
		return closeErr
	}
	if err != nil {
		return errors.Join(closeErr, err)
	}
	if os.SameFile(info, l.identity) {
		return errors.Join(closeErr, os.Remove(l.path))
	}
	return closeErr
}

func (s *Service) acceptLoop(ctx context.Context, l net.Listener, forwardID string) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.L.WithError(err).Error("socketforward: accept error")
			}
			return
		}
		go s.handleConnection(ctx, conn, forwardID)
	}
}

func (s *Service) handleConnection(ctx context.Context, udsConn net.Conn, forwardID string) {
	streamID, err := GenerateStreamID("socketfwd")
	if err != nil {
		log.L.WithError(err).Error("socketforward: generating stream ID")
		udsConn.Close()
		return
	}

	log.L.WithFields(log.Fields{
		"stream_id":  streamID,
		"forward_id": forwardID,
	}).Debug("socketforward: new forwarded connection")

	resultCh := make(chan error, 1)
	s.mu.Lock()
	s.pending[streamID] = resultCh
	s.mu.Unlock()

	req := &socketforward.ConnectRequest{
		StreamID:  streamID,
		ForwardID: forwardID,
	}
	select {
	case s.notify <- req:
	default:
		s.mu.Lock()
		delete(s.pending, streamID)
		s.mu.Unlock()
		log.G(ctx).WithFields(log.Fields{
			"stream_id":  streamID,
			"forward_id": forwardID,
		}).Error("socketforward: notify channel full, dropping connection")
		udsConn.Close()
		return
	}

	// Wait for the host to report whether it could dial the target socket.
	var dialErr error
	select {
	case dialErr = <-resultCh:
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, streamID)
		s.mu.Unlock()
		udsConn.Close()
		return
	}

	if dialErr != nil {
		log.L.WithError(dialErr).WithFields(log.Fields{
			"stream_id":  streamID,
			"forward_id": forwardID,
		}).Error("socketforward: host failed to dial target socket")
		udsConn.Close()
		return
	}

	// The host opens the vsock stream before sending the success
	// ConnectResult, so the stream is already registered by the time we
	// reach here.
	vsockConn, err := s.streams.Get(streamID)
	if err != nil {
		log.L.WithError(err).WithField("stream_id", streamID).Error("socketforward: vsock stream not found after successful ConnectResult")
		udsConn.Close()
		return
	}

	relay(ctx, udsConn, vsockConn)
}

// relay copies data bidirectionally between two connections until one side
// closes or errors. Both connections are closed when the relay finishes.
func relay(ctx context.Context, a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)

	// Wait for one direction to finish, then tear down both.
	select {
	case <-done:
	case <-ctx.Done():
	}
	a.Close()
	b.Close()
	<-done // wait for the second goroutine
}

// Shutdown closes all active listeners.
func (s *Service) Shutdown(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, l := range s.listeners {
		if err := l.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	s.listeners = nil
	return errors.Join(errs...)
}
