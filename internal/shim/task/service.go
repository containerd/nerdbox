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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	runcOptions "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"

	bundleAPI "github.com/containerd/nerdbox/api/services/bundle/v1"
	mountAPI "github.com/containerd/nerdbox/api/services/mount/v1"
	"github.com/containerd/nerdbox/api/services/vmevents/v1"
	"github.com/containerd/nerdbox/internal/nwcfg"
	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

var (
	_     = shim.TTRPCService(&service{})
	empty = &ptypes.Empty{}
)

// guestRuncOptions constructs a fresh runc Options message containing only the
// fields that are meaningful inside the VM guest, and returns it as a
// marshalled Any suitable for forwarding to vminitd.
//
// The runc Options proto conflates two distinct concerns:
//
//   - Host-side shim configuration: ShimCgroup, IoUid, IoGid, BinaryName, Root,
//     and TaskApiAddress. These reference host UID/GID values and host filesystem
//     paths and have no meaning inside the VM.
//
//   - Guest-side container configuration: NoPivotRoot, NoNewKeyring.
//     These affect how the OCI runtime inside the VM sets up the container
//     and are forwarded.
//
// Fields that are explicitly not forwarded, with a warning logged if set:
//
//   - SystemdCgroup: the VM has no systemd; the in-guest crun always uses the
//     cgroupsv2 fs driver.
//   - CriuImagePath / CriuWorkPath: host filesystem paths for checkpoint images;
//     checkpoint/restore is coordinated at the VM level by the shim.
//
// A new Options message is always constructed rather than filtering the incoming
// one, so that any unrecognised or future fields default to zero inside the guest.
// If opts is nil or empty, an empty Options is returned (zero values).
// If opts is not a runc Options message an error is returned; the shim knows the
// guest runtime and passing through an opaque blob would be unsafe.
func guestRuncOptions(ctx context.Context, opts *ptypes.Any) (*ptypes.Any, error) {
	out := &runcOptions.Options{}

	if opts.GetTypeUrl() != "" {
		v, err := typeurl.UnmarshalAny(opts)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to decode task options: %w", errdefs.ErrInvalidArgument, err)
		}
		src, ok := v.(*runcOptions.Options)
		if !ok {
			return nil, fmt.Errorf("%w: unsupported options type %T, guest runtime only supports runc options", errdefs.ErrInvalidArgument, v)
		}

		// Allow-listed fields that are meaningful inside the VM.
		out.NoPivotRoot = src.NoPivotRoot
		out.NoNewKeyring = src.NoNewKeyring

		// Warn about fields that were set but will not be forwarded, so the
		// caller knows the guest will not honour them.
		if src.SystemdCgroup {
			log.G(ctx).Warn("task options: SystemdCgroup ignored — the VM guest has no systemd; crun uses the cgroupsv2 fs driver")
		}
		if src.CriuImagePath != "" {
			log.G(ctx).WithField("criu_image_path", src.CriuImagePath).Warn("task options: CriuImagePath ignored — checkpoint/restore is coordinated at the VM level")
		}
		if src.CriuWorkPath != "" {
			log.G(ctx).WithField("criu_work_path", src.CriuWorkPath).Warn("task options: CriuWorkPath ignored — checkpoint/restore is coordinated at the VM level")
		}
	}

	a, err := typeurl.MarshalAny(out)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal guest runc options: %w", err)
	}
	return typeurl.MarshalProto(a), nil
}

// NewTaskService creates a new instance of a task service
func NewTaskService(ctx context.Context, sb sandbox.Sandbox, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	var debug bool
	if opts, ok := ctx.Value(shim.OptsKey{}).(shim.Opts); ok {
		debug = opts.Debug
	}

	s := &service{
		context:          ctx,
		sb:               sb,
		events:           make(chan any, 128),
		containers:       make(map[string]*container),
		debug:            debug,
		initiateShutdown: sd.Shutdown,
		shutdownDone:     sd.Done(),
	}
	sd.RegisterCallback(s.shutdown)

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			return shim.RemoveSocket(address)
		})
	}

	go s.forward(ctx, publisher)

	return s, nil
}

type container struct {
	ioShutdown func(context.Context) error
	// ioDone is closed when the host-side copy goroutines for the init
	// process have fully drained output to the destination FIFO.
	ioDone <-chan struct{}
	// stdinEOF, when non-nil, drains the host stdin FIFO and sends
	// OP_SHUTDOWN(SEND) in-order on the stdin stream. Called by CloseIO with
	// the CloseIO request context instead of forwarding the RPC out-of-band,
	// guaranteeing the EOF arrives after all in-flight stdin bytes. It blocks
	// until the EOF is delivered or the context is cancelled.
	stdinEOF func(context.Context) error

	// forwarder is the UNIX socket forwarder for this specific container.
	forwarder *socketForwarder

	execShutdowns map[string]func(context.Context) error
	// execIODone holds the ioDone channel for each exec's host-side copy
	// goroutines. The host Wait handler blocks on this before returning so
	// that all output bytes are guaranteed to be in the destination FIFO
	// before the caller can issue Delete.
	execIODone map[string]<-chan struct{}
	// execStdinEOF holds the in-band stdin EOF sender per exec ID.
	execStdinEOF map[string]func(context.Context) error
}

// shutdown shuts down the container's IO streams, socket forwarding, and all
// exec IO streams.
func (c *container) shutdown(ctx context.Context) error {
	var errs []error
	if c.ioShutdown != nil {
		if err := c.ioShutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("io shutdown: %w", err))
		}
	}
	if c.forwarder != nil {
		if err := c.forwarder.shutdown(); err != nil {
			errs = append(errs, fmt.Errorf("socket forward shutdown: %w", err))
		}
	}
	for execID, ioShutdown := range c.execShutdowns {
		if err := ioShutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("exec %q io shutdown: %w", execID, err))
		}
	}
	return errors.Join(errs...)
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	mu sync.Mutex

	// sb is the sandbox instance used to run the container
	sb sandbox.Sandbox

	context context.Context
	events  chan any

	containers map[string]*container

	debug                bool
	initiateShutdown     func()
	initiateShutdownOnce sync.Once
	shutdownDone         <-chan struct{}
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

func (s *service) shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error

	for id, c := range s.containers {
		if err := c.shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("container %q shutdown: %w", id, err))
		}
	}

	if s.sb != nil {
		// Unmount all block volumes inside the guest before stopping the VM,
		// to flush ext4 journals and dirty pages to the virtio-blk devices.
		// Best-effort with a short retry for transient EBUSY.
		if vmc, err := s.sb.Client(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to get VM client; skipping unmount of block volumes before VM shutdown")
		} else {
			if err := unmountAllWithRetry(ctx, mountAPI.NewTTRPCMountClient(vmc)); err != nil {
				log.G(ctx).WithError(err).Warn("failed to unmount all block volumes before VM shutdown")
			}
		}

		if err := s.sb.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("sandbox shutdown: %w", err))
		}
	}

	// Remove the rootfs directory on Windows so containerd's bundle cleanup
	// doesn't attempt a bind filter unmount (no-op on other platforms).
	removeRootfsDir(ctx)

	// Signal last event and stop forwarding
	s.events <- nil

	return errors.Join(errs...)
}

// unmountAllWithRetry asks the guest to unmount all tracked mounts, retrying
// briefly on transient failures (e.g. EBUSY). Permanent errors — including
// Unimplemented, connection errors, and context cancellation — stop the
// retry immediately.
func unmountAllWithRetry(ctx context.Context, mc mountAPI.TTRPCMountService) error {
	for {
		_, err := mc.UnmountAll(ctx, &mountAPI.UnmountAllRequest{})
		if err == nil {
			return nil
		}
		// Convert gRPC/TTRPC status errors to errdefs so IsNotImplemented works.
		native := errgrpc.ToNative(err)
		// Stop immediately on permanent errors: connection gone or method not
		// implemented. Only retry transient errors (e.g. EBUSY from the guest).
		if errors.Is(err, ttrpc.ErrClosed) || errors.Is(err, io.EOF) ||
			errdefs.IsNotImplemented(native) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Create a new initial process and container with the underlying OCI runtime
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	log.G(ctx).WithFields(log.Fields{
		"container_id": r.ID,
		"bundle":       r.Bundle,
		"rootfs":       r.Rootfs,
		"stdin":        r.Stdin,
		"stdout":       r.Stdout,
		"stderr":       r.Stderr,
	}).Info("creating container task")

	if r.Checkpoint != "" || r.ParentCheckpoint != "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("checkpoints not supported: %w", errdefs.ErrNotImplemented))
	}

	presetup := time.Now()

	var (
		nwpr        networksProvider
		ctrNetCfg   ctrNetConfig
		resCfg      resourceConfig
		dumpInfoCfg dumpInfoConfig
		bm          bindMounter
		sfpr        = socketForwardsProvider{containerID: r.ID}
	)
	// Load the OCI bundle and apply transformers to get the bundle that'll be
	// set up on the VM side.
	b, err := bundle.Load(ctx, r.Bundle,
		bm.FromBundle,
		nwpr.FromBundle,
		ctrNetCfg.fromBundle,
		resCfg.FromBundle,
		dumpInfoCfg.FromBundle,
		sfpr.FromBundle,
		func(ctx context.Context, b *bundle.Bundle) error {
			// If there are no VM networks, try falling back to host's resolv.conf (for TSI).
			return addResolvConf(ctx, b, len(nwpr.nws) == 0)
		},
	)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	nwJSON, err := json.Marshal(ctrNetCfg)
	if err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("marshaling container networking config: %w", err))
	}
	b.AddExtraFile(nwcfg.Filename, nwJSON)

	// vmState directory should be a path under the current working directory, not specific to a single bundle
	vmState, err := filepath.Abs("vm")
	if err != nil {
		return nil, errgrpc.ToGRPCf(err, "failed to get absolute path for vm state directory")
	}
	if err := os.MkdirAll(vmState, 0700); err != nil {
		return nil, errgrpc.ToGRPCf(err, "failed to create vm state directory %q", vmState)
	}

	// da is shared across rootfs and volume disk allocation so that all
	// virtio-block devices within a container get unique, sequential letters.
	// Start after the disks the VM implementation reserves for its own use
	// (e.g. the erofs rootfs at /dev/vda in the libkrun backend).
	reservedDisks := s.sb.ReservedDisks()
	da := newDiskAllocator(reservedDisks)
	m, mountOpts, err := setupMounts(ctx, r.ID, r.Rootfs, b.Rootfs, filepath.Join(r.Bundle, "mounts"), &da, r.Bundle)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Process ext4 volume mounts in the OCI spec after rootfs disks have been
	// allocated, so that rootfs disks always precede volume disks.
	var blockM blockMounter
	if err := blockM.FromBundle(ctx, b, r.ID, &da); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Enforce the virtio-block letter limit (vda–vdz = 26 devices total).
	// count() reflects only container disks; add the reserved range to get
	// the true total and compare against the full alphabet.
	if total := reservedDisks + da.count(); total > 26 {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "exceeded maximum virtio disk count: %d > 26", total)
	}

	var opts []sandbox.Opt
	if s.debug {
		opts = append(opts, sandbox.WithInitArgs("-debug"))
	}

	opts = append(opts, sandbox.WithStateDir(vmState))
	opts = append(opts, mountOpts...)

	opts = append(opts, nwpr.SandboxOptions()...)
	opts = append(opts, sandbox.WithInitArgs(nwpr.InitArgs()...))

	opts = append(opts, bm.SandboxOpts()...)
	opts = append(opts, blockM.SandboxOpts()...)

	opts = append(opts, resCfg.SandboxOpts()...)
	opts = append(opts, dumpInfoCfg.SandboxOpts()...)

	premountTime := time.Since(presetup)

	prestart := time.Now()
	if err := s.sb.Start(ctx, opts...); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	bootTime := time.Since(prestart)
	log.G(ctx).WithFields(log.Fields{
		"t_boot":     bootTime,
		"t_premount": premountTime,
	}).Info("VM started")

	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Start forwarding events.
	// Use the shim's long-lived context (not the RPC ctx) for the event
	// stream. If the connection closes, ctx gets canceled, which causes
	// RecvMsg to return without deleting the underlying ttrpc stream. The VM
	// keeps sending events to that orphaned stream, which fills the stream's
	// recv buffer and blocks the ttrpc receive loop — deadlocking all
	// subsequent calls on the same ttrpc client. This needs a fix in ttrpc
	// to avoid deadlock, but the stream should be consumed until the stream
	// is done or the ttrpc connection closes.
	sc, err := vmevents.NewTTRPCEventsClient(vmc).Stream(s.context, empty)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	ns, _ := namespaces.Namespace(ctx)
	go func(ns string) {
		for {
			ev, err := sc.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, shutdown.ErrShutdown) {
					log.G(ctx).Info("vm event stream closed")
				} else {
					log.G(ctx).WithError(err).Error("vm event stream error")
				}
				return
			}
			s.send(ev)
		}
	}(ns)

	bundleFiles, err := b.Files()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	bundleService := bundleAPI.NewTTRPCBundleClient(vmc)
	br, err := bundleService.Create(ctx, &bundleAPI.CreateRequest{
		ID:    r.ID,
		Files: bundleFiles,
	})
	if err != nil {
		return nil, err
	}

	var mountSpecs []*mountAPI.MountSpec
	for _, m := range append(bm.VmMounts(), blockM.VmMounts()...) {
		mountSpecs = append(mountSpecs, &mountAPI.MountSpec{
			Type:    m.Type,
			Source:  m.Source,
			Target:  m.Target,
			Options: m.Options,
		})
	}
	if len(mountSpecs) > 0 {
		if _, err := mountAPI.NewTTRPCMountClient(vmc).MountAll(ctx, &mountAPI.MountAllRequest{
			Mounts: mountSpecs,
		}); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
	}

	rio := stdio.Stdio{
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Terminal: r.Terminal,
	}

	cio, ioShutdown, initIODone, initStdinEOF, err := s.forwardIO(ctx, s.sb, r.ID, rio)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Bind socket forwards on the VM before container creation so
	// that crun can bind-mount the listener sockets into the container.
	if err := bindSockets(ctx, s.sb, sfpr.entries); err != nil {
		if err := ioShutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Error("failed to shutdown io after socket forwarding failure")
		}
		return nil, errgrpc.ToGRPC(err)
	}

	// setupTime is the total time to setup the VM and everything needed
	// to proxy the create task request. This measures the overall
	// overhead of creating the container inside the VM.
	setupTime := time.Since(presetup)

	preCreate := time.Now()
	c := &container{
		ioShutdown:    ioShutdown,
		ioDone:        initIODone,
		stdinEOF:      initStdinEOF,
		execShutdowns: make(map[string]func(context.Context) error),
		execIODone:    make(map[string]<-chan struct{}),
		execStdinEOF:  make(map[string]func(context.Context) error),
	}

	guestOpts, err := guestRuncOptions(ctx, r.Options)
	if err != nil {
		if err := ioShutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Error("failed to shutdown io after options error")
		}
		return nil, errgrpc.ToGRPC(fmt.Errorf("invalid task options: %w", err))
	}

	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Create(ctx, &taskAPI.CreateTaskRequest{
		ID:       r.ID,
		Bundle:   br.Bundle,
		Rootfs:   m,
		Terminal: cio.Terminal,
		Stdin:    cio.Stdin,
		Stdout:   cio.Stdout,
		Stderr:   cio.Stderr,
		Options:  guestOpts,
	})
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to create task")
		if err := c.shutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Error("failed to shutdown container after create failure")
		}
		return nil, errgrpc.ToGRPC(err)
	}

	// Start the Accept stream after the container has been created so the
	// host can relay forwarded connections from the VM.
	fwder, err := startSocketForwarding(context.Background(), s.sb, r.ID, sfpr.entries)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to start socket forwarding")
		if err := c.shutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Error("failed to shutdown container after socket forwarding failure")
		}
		return nil, errgrpc.ToGRPC(err)
	}
	c.forwarder = fwder

	log.G(ctx).WithFields(log.Fields{
		"t_boot":   bootTime,
		"t_setup":  setupTime - bootTime,
		"t_create": time.Since(preCreate),
	}).Info("task successfully created")

	s.mu.Lock()
	s.containers[r.ID] = c
	s.mu.Unlock()

	// TODO: Forward events rather than generate here?
	//s.send(&eventstypes.TaskCreate{
	//	ContainerID: r.ID,
	//	Bundle:      r.Bundle,
	//	Rootfs:      r.Rootfs,
	//	IO: &eventstypes.TaskIO{
	//		Stdin:    r.Stdin,
	//		Stdout:   r.Stdout,
	//		Stderr:   r.Stderr,
	//		Terminal: r.Terminal,
	//	},
	//	Pid: resp.Pid,
	//})

	// The following line cannot return an error as the only state in which that
	// could happen would also cause the container.Pid() call above to
	// nil-deference panic.
	// proc, _ := container.Process("")
	// handleStarted(container, proc)

	return &taskAPI.CreateTaskResponse{
		Pid: resp.Pid,
	}, nil
}

// Start a process
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID, "exec_id": r.ExecID}).Info("starting container task")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Start(ctx, r)
}

// Delete the initial process and container
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID, "exec_id": r.ExecID}).Info("deleting task")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Delete(ctx, r)
	if err == nil {
		s.mu.Lock()
		if c, ok := s.containers[r.ID]; ok {
			if r.ExecID != "" {
				if ioShutdown, ok := c.execShutdowns[r.ExecID]; ok {
					if err := ioShutdown(ctx); err != nil {
						log.G(ctx).WithError(err).WithField("exec_id", r.ExecID).Error("failed to shutdown exec io after delete")
					}
					delete(c.execShutdowns, r.ExecID)
					delete(c.execIODone, r.ExecID)
					delete(c.execStdinEOF, r.ExecID)
				}
			} else {
				if err := c.shutdown(ctx); err != nil {
					log.G(ctx).WithError(err).Error("failed to shutdown container after delete")
				}
				delete(s.containers, r.ID)
			}
		}
		s.mu.Unlock()

	}
	return resp, err
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID, "exec_id": r.ExecID}).Info("exec container")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	rio := stdio.Stdio{
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Terminal: r.Terminal,
	}

	cio, ioShutdown, ioDone, stdinEOF, err := s.forwardIO(ctx, s.sb, r.ID+"-"+r.ExecID, rio)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	s.mu.Lock()
	c, ok := s.containers[r.ID]
	if ok && ioShutdown != nil {
		c.execShutdowns[r.ExecID] = ioShutdown
		if ioDone != nil {
			c.execIODone[r.ExecID] = ioDone
		}
		if stdinEOF != nil {
			c.execStdinEOF[r.ExecID] = stdinEOF
		}
	}
	s.mu.Unlock()

	if !ok {
		// The container is gone, so nothing is tracking this exec's IO.
		// Shut it down here before sending back container not found.
		if ioShutdown != nil {
			if err := ioShutdown(ctx); err != nil {
				log.G(ctx).WithError(err).Error("failed to shutdown exec io after container not found")
			}
		}
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "container %q not found", r.ID)
	}

	vr := &taskAPI.ExecProcessRequest{
		ID:       r.ID,
		ExecID:   r.ExecID,
		Terminal: cio.Terminal,
		Stdin:    cio.Stdin,
		Stdout:   cio.Stdout,
		Stderr:   cio.Stderr,
		Spec:     r.Spec,
	}
	resp, err := taskAPI.NewTTRPCTaskClient(vmc).Exec(ctx, vr)
	if err != nil {
		// Take the tracked shutdown out under the lock, but run it after
		// unlocking so a slow shutdown doesn't block other task operations.
		s.mu.Lock()
		var execShutdown func(context.Context) error
		if c, ok := s.containers[r.ID]; ok {
			if f, ok := c.execShutdowns[r.ExecID]; ok {
				execShutdown = f
				delete(c.execShutdowns, r.ExecID)
			}
		}
		s.mu.Unlock()
		if execShutdown != nil {
			if err := execShutdown(ctx); err != nil {
				log.G(ctx).WithError(err).Error("failed to shutdown exec io after exec failure")
			}
		}
		return nil, err
	}

	return resp, err
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID, "exec_id": r.ExecID}).Info("resize pty")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.ResizePty(ctx, r)
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	st, err := tc.State(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"container_id": r.ID, "exec_id": r.ExecID}).Info("state")
		return nil, err
	}
	log.G(ctx).WithFields(log.Fields{"status": st.Status, "container_id": r.ID, "exec_id": r.ExecID}).Info("state")

	return st, err
}

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID}).Info("pause")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Pause(ctx, r)
}

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID}).Info("resume")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Resume(ctx, r)
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID, "exec_id": r.ExecID}).Info("kill")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Kill(ctx, r)
}

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID}).Info("all pids")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Pids(ctx, r)
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID, "exec_id": r.ExecID, "stdin": r.Stdin}).Info("close io")
	if r.Stdin {
		// Deliver stdin EOF in-band on the stream connection rather than
		// forwarding the RPC out-of-band. The in-band CloseWrite sends
		// OP_SHUTDOWN(SEND) ordered after all data already written to the
		// stream, preventing truncation caused by an out-of-band RPC on a
		// separate vsock connection racing in-flight stdin bytes.
		s.mu.Lock()
		var stdinEOF func(context.Context) error
		if c, ok := s.containers[r.ID]; ok {
			if r.ExecID != "" {
				stdinEOF = c.execStdinEOF[r.ExecID]
			} else {
				stdinEOF = c.stdinEOF
			}
		}
		s.mu.Unlock()

		if stdinEOF != nil {
			if err := stdinEOF(ctx); err != nil {
				log.G(ctx).WithError(err).WithFields(log.Fields{
					"container_id": r.ID,
					"exec_id":      r.ExecID,
				}).Error("failed to send stdin EOF")
				return nil, errgrpc.ToGRPC(err)
			}
			return empty, nil
		}
	}
	// Non-stdin CloseIO or no stream registered: forward to the guest.
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.CloseIO(ctx, r)
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID}).Info("checkpoint")
	/*
		container, err := s.getContainer(r.ID)
		if err != nil {
			return nil, err
		}
		if err := container.Checkpoint(ctx, r); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
	*/
	return empty, nil
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID}).Info("update")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Update(ctx, r)
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID, "exec_id": r.ExecID}).Info("wait")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Wait(ctx, r)
	if err != nil {
		return nil, err
	}
	// Block until the host-side copy goroutines have fully drained output
	// to the destination FIFO before returning the exit status to the
	// caller. This guarantees that by the time the caller sees the Wait
	// response, all process output is in the FIFO — so a subsequent
	// Delete cannot race with in-flight bytes still buffered in the
	// VM→host vsock transport.
	//
	// This mirrors the guarantee runc shims provide natively: with runc
	// the process output pipe is local, so EOF on the FIFO is synchronous
	// with process exit. With nerdbox the output travels through a
	// vsock→Unix socket bridge, introducing a transport lag that the
	// Wait response must account for.
	s.mu.Lock()
	var ioDone <-chan struct{}
	if c, ok := s.containers[r.ID]; ok {
		if r.ExecID != "" {
			ioDone = c.execIODone[r.ExecID]
		} else {
			ioDone = c.ioDone
		}
	}
	s.mu.Unlock()

	if ioDone != nil {
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		select {
		case <-ioDone:
		case <-drainCtx.Done():
			log.G(ctx).WithError(drainCtx.Err()).WithFields(log.Fields{
				"container_id": r.ID,
				"exec_id":      r.ExecID,
			}).Warn("timed out waiting for IO drain after wait")
		}
	}
	return resp, nil
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID}).Info("connect")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	vr, err := tc.Connect(ctx, r)
	if err != nil {
		return nil, err
	}

	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: vr.TaskPid,
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID}).Info("shutdown")

	// TODO: Should we forward this to VM?
	// tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	// return tc.Shutdown(ctx, r)

	s.initiateShutdownOnce.Do(s.initiateShutdown)

	select {
	case <-s.shutdownDone:
		return empty, nil
	case <-ctx.Done():
		return nil, errgrpc.ToGRPC(ctx.Err())
	}
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"container_id": r.ID}).Info("stats")
	vmc, err := s.sb.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Stats(ctx, r)
}

func (s *service) send(evt interface{}) {
	s.events <- evt
}

func (s *service) forward(ctx context.Context, publisher shim.Publisher) {
	ns, _ := namespaces.Namespace(ctx)
	ctx = namespaces.WithNamespace(context.Background(), ns)
	for e := range s.events {
		if e == nil {
			break
		}
		switch e := e.(type) {
		case *types.Envelope:
			// TODO: Transform event fields?
			if err := publisher.Publish(ctx, e.Topic, e.Event); err != nil {
				log.G(ctx).WithError(err).Error("forward event")
			}
		default:
			err := publisher.Publish(ctx, runtime.GetTopic(e), e)
			if err != nil {
				log.G(ctx).WithError(err).Error("post event")
			}
		}
	}
	publisher.Close()
	for e := range s.events {
		log.G(ctx).WithField("event", e).Error("ignored event after shutdown")
	}
}
