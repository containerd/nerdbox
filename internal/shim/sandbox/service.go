// Copyright The containerd Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// sandboxStateReady is returned by SandboxStatus once StartSandbox has
	// completed successfully. This must be exactly "SANDBOX_READY" — not a
	// human-readable state name — because containerd's CRI layer
	// (internal/cri/server/sandbox_status.go, toCRISandboxStatus) looks this
	// string up in runtime.PodSandboxState_value, the CRI v1
	// PodSandboxState enum's name-to-value map, to derive
	// PodSandboxStatus.State. Any string that isn't a name in that enum
	// (including a more "sensible" one like "ready") silently falls back to
	// SANDBOX_NOTREADY, so a real CRI client would see every sandbox as
	// permanently not-ready even while StartSandbox has succeeded and
	// containers are running in it.
	sandboxStateReady = "SANDBOX_READY"
	// sandboxStateStopped is returned after StopSandbox and before
	// StartSandbox has completed. The CRI v1 PodSandboxState enum has only
	// two values (ready / not ready) — there is no separate "stopped" vs
	// "never started" state — so both map to the same string here.
	sandboxStateStopped = "SANDBOX_NOTREADY"
)

// StartOptionsFunc is a callback that the task service registers with the
// SandboxService to provide VM start options (networking, resources, init
// args) derived from the sandbox OCI bundle. It is called during StartSandbox
// before the VM boots.
//
// Using a callback avoids a circular import between the sandbox and task
// packages: the task package owns bundle parsing; the sandbox package owns
// VM lifecycle.
type StartOptionsFunc func(ctx context.Context, bundlePath string) ([]Opt, error)

// SandboxService implements the containerd TTRPCSandboxService and is
// registered on the shim's TTRPC server alongside the Task service. It owns
// the VM lifecycle: CreateSandbox prepares the shared filesystem and VM
// configuration; StartSandbox boots the VM; StopSandbox/ShutdownSandbox tear
// it down.
//
// The task service acquires the already-running VM via the shared Sandbox
// interface and the SharedFS returned by FS().
type SandboxService struct {
	mu sync.Mutex

	sb       Sandbox   // underlying VM sandbox
	sharedFS *SharedFS // shared host↔guest filesystem tree

	// networkSandbox pins the host-side network isolation resource
	// (e.g. a Linux network namespace bind-mount) for the lifetime of the
	// sandbox.  It is set in CreateSandbox from CreateSandboxRequest.NetnsPath
	// and released in StopSandbox/ShutdownSandbox.
	networkSandbox NetworkSandbox

	// startOptsFn, if non-nil, is called in StartSandbox to get bundle-derived
	// VM start options (networking, resources, init args). Set by the task
	// plugin via RegisterStartOptions before Start is called.
	startOptsFn StartOptionsFunc

	// lifecycle state
	sandboxID  string
	bundlePath string
	stateDir   string
	pid        uint32
	createdAt  time.Time
	state      string // "" | sandboxStateReady | sandboxStateStopped
	exitCh     chan struct{}
	exitOnce   sync.Once

	// options holds CreateSandboxRequest.Options verbatim: an opaque,
	// caller-defined payload (in production CRI, a marshaled
	// k8s.io/cri-api PodSandboxConfig — see internal/cri/server's
	// sandbox_run.go, sandbox.WithOptions). The sandbox package
	// deliberately does not interpret it: unmarshaling CRI-specific types
	// is left to the task package (which already owns other CRI-shaped
	// concerns like DNS annotations), keeping this package's API surface
	// generic to the shim-v2 sandbox protocol rather than coupled to CRI.
	options *anypb.Any
}

var _ sandboxAPI.TTRPCSandboxService = (*SandboxService)(nil)

// NewSandboxService creates a SandboxService backed by the given Sandbox.
func NewSandboxService(sb Sandbox) *SandboxService {
	return &SandboxService{
		sb:     sb,
		exitCh: make(chan struct{}),
	}
}

// RegisterStartOptions installs a callback that SandboxService calls during
// StartSandbox to obtain bundle-derived VM options. The task plugin calls this
// at initialisation time before any sandbox RPCs arrive.
func (s *SandboxService) RegisterStartOptions(fn StartOptionsFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startOptsFn = fn
}

// RegisterTTRPC registers the sandbox service on the TTRPC server.
func (s *SandboxService) RegisterTTRPC(server *ttrpc.Server) error {
	sandboxAPI.RegisterTTRPCSandboxService(server, s)
	return nil
}

// FS returns the SharedFS associated with this sandbox, or nil if the sandbox
// has not been created yet. The task service uses this to share container
// rootfses into the VM.
func (s *SandboxService) FS() *SharedFS {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sharedFS
}

// IsSandboxed returns true once CreateSandbox has been called. The task
// service uses this to distinguish the sandbox API path from the legacy
// single-container path.
func (s *SandboxService) IsSandboxed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sandboxID != ""
}

// CreateSandbox is called by containerd right after the shim starts. It
// records the sandbox ID and bundle path, allocates the state directory, and
// creates the shared filesystem tree. The VM is NOT started here — that
// happens in StartSandbox.
func (s *SandboxService) CreateSandbox(ctx context.Context, req *sandboxAPI.CreateSandboxRequest) (*sandboxAPI.CreateSandboxResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.G(ctx).WithField("sandboxID", req.SandboxID).Info("CreateSandbox")

	if s.sandboxID != "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("sandbox already created: %w", errdefs.ErrAlreadyExists))
	}

	bundlePath := req.BundlePath
	if bundlePath == "" {
		// Fall back to the shim's current working directory.
		var err error
		bundlePath, err = os.Getwd()
		if err != nil {
			return nil, errgrpc.ToGRPC(fmt.Errorf("getwd: %w", err))
		}
	}

	// State lives under the shim working directory.
	stateDir, err := filepath.Abs("vm")
	if err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("abs vm state dir: %w", err))
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("create vm state dir: %w", err))
	}

	sharedFS, err := NewSharedFS(stateDir)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Open the host-side network sandbox (e.g. a Linux netns bind-mount).
	// The open FD pins the resource for the sandbox lifetime so that CNI
	// and other host-side tools can reference it while the sandbox runs.
	// An empty NetnsPath means host-network — openNetworkSandbox returns
	// a no-op NoNetworkSandbox in that case.
	ns, err := openNetworkSandbox(req.NetnsPath)
	if err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("open network sandbox: %w", err))
	}
	if ns.Path() != "" {
		log.G(ctx).WithFields(log.Fields{
			"sandboxID": req.SandboxID,
			"netns":     ns.Path(),
		}).Debug("network sandbox pinned")
	}

	s.sandboxID = req.SandboxID
	s.bundlePath = bundlePath
	s.stateDir = stateDir
	s.sharedFS = sharedFS
	s.networkSandbox = ns
	s.options = req.Options
	s.state = ""

	return &sandboxAPI.CreateSandboxResponse{}, nil
}

// Options returns CreateSandboxRequest.Options verbatim (nil if none was
// given, or CreateSandbox has not been called yet). See the field's doc
// comment on SandboxService for why this package does not interpret it.
func (s *SandboxService) Options() *anypb.Any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.options
}

// StartSandbox boots the VM. It calls the registered StartOptionsFunc (if
// any) to obtain bundle-derived options (networking, resources, init args),
// then adds the shared filesystem share and starts the VM.
func (s *SandboxService) StartSandbox(ctx context.Context, req *sandboxAPI.StartSandboxRequest) (*sandboxAPI.StartSandboxResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.G(ctx).WithField("sandboxID", req.SandboxID).Info("StartSandbox")

	if s.sandboxID == "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("sandbox not created: %w", errdefs.ErrFailedPrecondition))
	}
	if s.state == sandboxStateReady {
		return nil, errgrpc.ToGRPC(fmt.Errorf("sandbox already started: %w", errdefs.ErrAlreadyExists))
	}

	// Base options: state dir, the single shared virtiofs share, and the
	// pod network namespace path (empty = host-network / no namespace entry).
	opts := []Opt{
		WithStateDir(s.stateDir),
		WithFS(SharedFSTag, s.sharedFS.Root(), false),
		WithNetnsPath(s.networkSandbox.Path()),
	}

	// Append bundle-derived options (networking, resources, init args).
	if s.startOptsFn != nil {
		bundleOpts, err := s.startOptsFn(ctx, s.bundlePath)
		if err != nil {
			return nil, errgrpc.ToGRPC(fmt.Errorf("sandbox start options: %w", err))
		}
		opts = append(opts, bundleOpts...)
	}

	if err := s.sb.Start(ctx, opts...); err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("start VM: %w", err))
	}

	s.createdAt = time.Now()
	s.pid = uint32(os.Getpid())
	s.state = sandboxStateReady

	return &sandboxAPI.StartSandboxResponse{
		Pid:       s.pid,
		CreatedAt: timestamppb.New(s.createdAt),
	}, nil
}

// Platform returns the platform the sandbox runs containers on.
func (s *SandboxService) Platform(_ context.Context, _ *sandboxAPI.PlatformRequest) (*sandboxAPI.PlatformResponse, error) {
	return &sandboxAPI.PlatformResponse{
		Platform: &types.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		},
	}, nil
}

// StopSandbox stops the VM. It cleans up all host-side container mounts
// before shutting down the VM to ensure clean state.
func (s *SandboxService) StopSandbox(ctx context.Context, req *sandboxAPI.StopSandboxRequest) (*sandboxAPI.StopSandboxResponse, error) {
	log.G(ctx).WithField("sandboxID", req.SandboxID).Info("StopSandbox")

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != sandboxStateReady {
		return &sandboxAPI.StopSandboxResponse{}, nil
	}

	if s.sharedFS != nil {
		if err := s.sharedFS.UnshareAll(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("failed to unshare all containers on stop")
		}
	}

	if err := s.sb.Stop(ctx); err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("stop VM: %w", err))
	}

	// Release the network sandbox pin after the VM stops so that CNI can
	// run its teardown while the sandbox was still marked running.
	if s.networkSandbox != nil {
		if err := s.networkSandbox.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close network sandbox on stop")
		}
		s.networkSandbox = nil
	}

	s.state = sandboxStateStopped
	s.exitOnce.Do(func() { close(s.exitCh) })

	return &sandboxAPI.StopSandboxResponse{}, nil
}

// WaitSandbox blocks until the sandbox has exited.
func (s *SandboxService) WaitSandbox(ctx context.Context, req *sandboxAPI.WaitSandboxRequest) (*sandboxAPI.WaitSandboxResponse, error) {
	log.G(ctx).WithField("sandboxID", req.SandboxID).Debug("WaitSandbox")

	select {
	case <-s.exitCh:
	case <-ctx.Done():
		return nil, errgrpc.ToGRPC(ctx.Err())
	}

	return &sandboxAPI.WaitSandboxResponse{
		ExitStatus: 0,
		ExitedAt:   timestamppb.Now(),
	}, nil
}

// SandboxStatus returns the current status of the sandbox.
func (s *SandboxService) SandboxStatus(_ context.Context, req *sandboxAPI.SandboxStatusRequest) (*sandboxAPI.SandboxStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := s.state
	if state == "" {
		// Created but not yet started: also not-ready, per the same
		// SANDBOX_READY/SANDBOX_NOTREADY contract documented on
		// sandboxStateReady above.
		state = sandboxStateStopped
	}

	// Populate the info map with observable sandbox metadata so that
	// callers (CRI, tests) can inspect sandbox state without additional
	// side-channel calls.
	info := map[string]string{
		"state": state,
		"pid":   fmt.Sprintf("%d", s.pid),
	}
	if s.networkSandbox != nil && s.networkSandbox.Path() != "" {
		info["networkSandboxPath"] = s.networkSandbox.Path()
	}

	return &sandboxAPI.SandboxStatusResponse{
		SandboxID: req.SandboxID,
		Pid:       s.pid,
		State:     state,
		Info:      info,
		CreatedAt: timestamppb.New(s.createdAt),
	}, nil
}

// PingSandbox is a lightweight liveness check.
func (s *SandboxService) PingSandbox(_ context.Context, _ *sandboxAPI.PingRequest) (*sandboxAPI.PingResponse, error) {
	return &sandboxAPI.PingResponse{}, nil
}

// ShutdownSandbox fully tears down the sandbox. containerd calls this after
// StopSandbox.
func (s *SandboxService) ShutdownSandbox(ctx context.Context, req *sandboxAPI.ShutdownSandboxRequest) (*sandboxAPI.ShutdownSandboxResponse, error) {
	log.G(ctx).WithField("sandboxID", req.SandboxID).Info("ShutdownSandbox")

	if _, err := s.StopSandbox(ctx, &sandboxAPI.StopSandboxRequest{SandboxID: req.SandboxID}); err != nil {
		log.G(ctx).WithError(err).Warn("ShutdownSandbox: stop failed")
	}

	return &sandboxAPI.ShutdownSandboxResponse{}, nil
}

// SandboxMetrics returns metrics for the sandbox.
func (s *SandboxService) SandboxMetrics(_ context.Context, _ *sandboxAPI.SandboxMetricsRequest) (*sandboxAPI.SandboxMetricsResponse, error) {
	return nil, errgrpc.ToGRPC(fmt.Errorf("metrics not implemented: %w", errdefs.ErrNotImplemented))
}

// ── Sandbox interface delegation ──────────────────────────────────────────────
// SandboxService implements the Sandbox interface by delegating to the inner
// sandbox VM. This allows the task service to accept a *SandboxService and
// use it both as a Sandbox (for VM communication) and as a SandboxService
// (for SharedFS access and lifecycle state).

// Start implements Sandbox. The task service calls this on the legacy
// single-container path where no CreateSandbox/StartSandbox RPCs arrive.
func (s *SandboxService) Start(ctx context.Context, opts ...Opt) error {
	return s.sb.Start(ctx, opts...)
}

// Stop implements Sandbox.
func (s *SandboxService) Stop(ctx context.Context) error {
	return s.sb.Stop(ctx)
}

// Client implements Sandbox. Returns the TTRPC client connected to vminitd.
func (s *SandboxService) Client() (*ttrpc.Client, error) {
	return s.sb.Client()
}

// StartStream implements Sandbox.
func (s *SandboxService) StartStream(ctx context.Context, streamID string) (net.Conn, error) {
	return s.sb.StartStream(ctx, streamID)
}

// ReservedDisks implements Sandbox.
func (s *SandboxService) ReservedDisks() int {
	return s.sb.ReservedDisks()
}
