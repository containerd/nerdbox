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

// Package initd contains the runnable entry point for the vminitd guest
// process. External cmd wrappers parse flags via ParseFlags and call Run
// to start the service. Plugins are registered via blank imports in the
// cmd wrapper.
package initd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/sys/reaper"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/otelttrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"golang.org/x/sys/unix"

	"github.com/containerd/nerdbox/internal/systools"
	"github.com/containerd/nerdbox/internal/vminit/podnetns"
	"github.com/containerd/nerdbox/internal/vminit/vmnetworking"
	"github.com/containerd/nerdbox/plugins"
)

// logLevel controls the slog handler level for vminitd.
var logLevel = &slog.LevelVar{}

// ExtraServerInterceptors are chained after the default otelttrpc interceptor
// when the vminitd ttrpc server starts. Callers may append to this slice from
// a package init() function before initd.Run is called to inject additional
// server-side interceptors (e.g. for distributed tracing).
var ExtraServerInterceptors []ttrpc.UnaryServerInterceptor

func init() {
	log.UseSlog()
	// Write structured logs to /dev/console rather than stderr so that
	// output does not end up in the kernel message buffer (kmsg).
	console, err := os.OpenFile("/dev/console", os.O_WRONLY, 0644)
	if err != nil {
		console = os.Stderr
	}
	handler := slog.NewJSONHandler(console, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(handler).With("component", "vminitd"))
}

// Config holds the parsed vminitd configuration.
type Config struct {
	VSockContextID int
	RPCPort        int
	StreamPort     int
	Networks       Networks
	DumpInfo       bool
	Debug          bool
	Dev            bool
}

// ParseFlags parses the standard vminitd command-line flags from args and
// returns the populated Config. The "tsi_hijack" leading argument injected
// by libkrun (when TSI is enabled) is stripped before parsing.
func ParseFlags(args []string) Config {
	var config Config
	fs := flag.NewFlagSet("vminitd", flag.ExitOnError)
	fs.BoolVar(&config.Debug, "debug", false, "Debug log level")
	fs.IntVar(&config.RPCPort, "vsock-rpc-port", 1024, "vsock port to listen for rpc on")
	fs.IntVar(&config.StreamPort, "vsock-stream-port", 1025, "vsock port to listen for streams on")
	fs.IntVar(&config.VSockContextID, "vsock-cid", 0, "vsock context ID for vsock listen")
	fs.Var(&config.Networks, "network", "network interfaces to set up")
	fs.BoolVar(&config.DumpInfo, "dump-info", false, "dump information about the system")
	fs.BoolVar(&config.Dev, "dev", false, "Development mode with graceful exit")

	if len(args) > 0 && args[0] == "tsi_hijack" {
		args = args[1:]
	}
	_ = fs.Parse(args)
	return config
}

// Run starts vminitd: performs system init (mounts, cgroups, networking),
// builds the ttrpc service from registered plugins, and runs until a fatal
// error or shutdown signal. Plugin _ imports must be wired in by the caller.
func Run(ctx context.Context) error {
	t1 := time.Now()
	config := ParseFlags(os.Args[1:])

	if config.Dev || config.Debug {
		logLevel.Set(slog.LevelDebug)
		log.SetLevel("debug")
	}

	log.G(ctx).WithField("env", os.Environ()).Debug("starting vminitd")

	var runErr error
	defer func() {
		switch {
		case runErr != nil:
			log.G(ctx).WithError(runErr).Error("exiting with error")
		default:
			if p := recover(); p != nil {
				log.G(ctx).WithField("panic", p).Error("recovered from panic")
			} else {
				log.G(ctx).Debug("exiting cleanly")
			}
		}
		if !config.Dev {
			log.G(ctx).Debug("poweroff")
		}
	}()

	ctx, shutdownSvc := shutdown.WithShutdown(ctx)

	if err := systemInit(ctx, config, shutdownSvc); err != nil {
		runErr = err
		return err
	}

	if config.DumpInfo {
		systools.DumpInfo(ctx)
	}

	svc, err := newService(ctx, config, shutdownSvc)
	if err != nil {
		runErr = err
		return err
	}

	log.G(ctx).WithField("t_init", time.Since(t1)).Debug("initialized vminitd")

	runtime.GOMAXPROCS(2)

	serviceErr := make(chan error, 1)
	go func() {
		serviceErr <- svc.Run(ctx)
	}()

	s := make(chan os.Signal, 16)
	signal.Notify(s, unix.SIGKILL, unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT, unix.SIGCHLD)
	for {
		select {
		case <-shutdownSvc.Done():
			if err := shutdownSvc.Err(); err != nil && !errors.Is(err, shutdown.ErrShutdown) {
				log.G(ctx).WithError(err).Error("shutdown error")
			}
			return nil
		case e := <-serviceErr:
			log.G(ctx).WithError(e).Error("service exited")
			runErr = e
			return e
		case sig := <-s:
			switch sig {
			case unix.SIGCHLD:
				if err := reaper.Reap(); err != nil {
					log.G(ctx).WithError(err).Error("failed to reap child process")
				} else {
					log.G(ctx).Debug("reaped child process")
				}
			case unix.SIGKILL, unix.SIGINT, unix.SIGTERM, unix.SIGQUIT:
				shutdownSvc.Shutdown()
				log.G(ctx).WithField("signal", sig).Info("received shutdown signal")
			default:
				log.G(ctx).WithField("signal", sig).Debug("received unhandled signal")
			}
		}
	}
}

func systemInit(ctx context.Context, config Config, shutdownSvc shutdown.Service) error {
	t := time.Now()

	// Raise the open-file-descriptor limit for the init process and all
	// children.  The kernel default (1024) is too low for long-running
	// sandbox sessions under sustained container churn.
	//
	// Each container's OOM monitor (oomv2.Add) creates one inotify FD via
	// cgroup2.Manager.EventChan and spawns a short-lived goroutine that holds
	// it until the goroutine is scheduled and completes (microseconds of work).
	// Under heavy load with GOMAXPROCS=2, the Go scheduler may not immediately
	// service these goroutines, allowing a burst of unscheduled goroutines to
	// accumulate.  Each holds one inotify FD until it runs.  At ~36 container
	// starts/second the burst can briefly hold hundreds of FDs before the
	// scheduler catches up.  65536 gives ~1800 seconds of headroom at that
	// rate — far beyond any scheduling stall in practice.
	nofileLimit := unix.Rlimit{Cur: 65536, Max: 65536}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &nofileLimit); err != nil {
		log.G(ctx).WithError(err).Warn("failed to raise RLIMIT_NOFILE; FD exhaustion may occur under sustained load")
	}

	if err := systemMounts(); err != nil {
		return err
	}

	if err := setupCgroupControl(); err != nil {
		return err
	}

	dhcpRenewer, dhcpReleaser, err := vmnetworking.SetupVM(ctx, config.Networks, config.Debug)
	if err != nil {
		return err
	}

	// Create the persistent, shared network namespace that sandbox member
	// containers join by default (see internal/podnetns for why). This is
	// independent of the VM's own root network namespace set up above by
	// vmnetworking.SetupVM.
	if err := podnetns.Create(ctx); err != nil {
		return err
	}

	shutdownSvc.RegisterCallback(func(ctx context.Context) error {
		return dhcpReleaser()
	})

	go func() {
		if err := dhcpRenewer(ctx); err != nil {
			log.G(ctx).WithError(err).Error("failed to renew DHCP leases")
			shutdownSvc.Shutdown()
		}
	}()

	log.G(ctx).WithField("elapsed", time.Since(t)).Info("system init completed")
	return nil
}

func systemMounts() error {
	required := []mount.Mount{
		{
			Type:    "proc",
			Source:  "proc",
			Target:  "/proc",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:    "sysfs",
			Source:  "sysfs",
			Target:  "/sys",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:   "cgroup2",
			Source: "none",
			Target: "/sys/fs/cgroup",
		},
		{
			Type:    "tmpfs",
			Source:  "tmpfs",
			Target:  "/run",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:    "tmpfs",
			Source:  "tmpfs",
			Target:  "/tmp",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		// Mount a tmpfs over /etc so that runtime writes (resolv.conf,
		// hosts, etc.) succeed even though the erofs rootfs is read-only.
		// The /etc directory is pre-created in the erofs image as a mount
		// point.
		{
			Type:    "tmpfs",
			Source:  "tmpfs",
			Target:  "/etc",
			Options: []string{"nosuid", "noexec", "nodev", "mode=755"},
		},
		// /dev is handled by the kernel via CONFIG_DEVTMPFS_MOUNT=y before
		// the init process starts; no explicit mount is needed here.
	}

	if err := mount.All(required, "/"); err != nil {
		return err
	}

	// Mount the sandbox container-shared virtiofs at /run/containers.
	// The host shim assembles each container's rootfs under
	// <sandbox-state>/containers/<id>/rootfs and exposes it through this
	// single share tagged "containers". This mount is optional: on the legacy
	// single-container path the "containers" tag is not registered by the host
	// and the mount will fail. We ignore the error so the legacy path is
	// unaffected.
	//
	// /run/containers is created at runtime (under the /run tmpfs) so no
	// change to the erofs rootfs image is required.
	mountContainersFS()

	return nil
}

func setupCgroupControl() error {
	return os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+cpu +cpuset +io +memory +pids"), 0644)
}

// ttrpcService allows TTRPC services to be registered with the underlying server
type ttrpcService interface {
	RegisterTTRPC(*ttrpc.Server) error
}

type service struct {
	l      net.Listener
	server *ttrpc.Server
}

func (s *service) Run(ctx context.Context) error {
	return s.server.Serve(ctx, s.l)
}

func newService(ctx context.Context, config Config, shutdownSvc shutdown.Service) (*service, error) {
	var (
		initializedPlugins = plugin.NewPluginSet()
		pluginProperties   = map[string]string{
			plugins.PropertyBundleDir: "/run/bundles",
		}
		disabledPlugins = map[string]struct{}{}
	)

	// Dial back to the host via vsock instead of listening. The host shim
	// is listening for this connection. CID 2 is the well-known host CID.
	const hostCID = 2
	l := newDialBackListener(uint32(hostCID), uint32(config.RPCPort))
	shutdownSvc.RegisterCallback(func(ctx context.Context) error {
		return l.Close()
	})

	ts, err := ttrpc.NewServer(
		ttrpc.WithChainUnaryServerInterceptor(
			append([]ttrpc.UnaryServerInterceptor{otelttrpc.UnaryServerInterceptor()},
				ExtraServerInterceptors...)...),
	)
	if err != nil {
		return nil, err
	}
	shutdownSvc.RegisterCallback(ts.Shutdown)

	registry.Register(&plugin.Registration{
		Type: cplugins.InternalPlugin,
		ID:   "shutdown",
		InitFn: func(ic *plugin.InitContext) (any, error) {
			return shutdownSvc, nil
		},
	})

	for _, reg := range registry.Graph(func(*plugin.Registration) bool { return false }) {
		id := reg.URI()
		if _, ok := disabledPlugins[id]; ok {
			log.G(ctx).WithField("plugin_id", id).Info("plugin is disabled, skipping load")
			continue
		}

		log.G(ctx).WithField("plugin_id", id).Info("loading plugin")

		ic := plugin.NewContext(ctx, initializedPlugins, pluginProperties)

		if reg.Config != nil {
			if vc, ok := reg.Config.(interface{ SetVsock(uint32, uint32) }); ok {
				if reg.Type == plugins.StreamingPlugin {
					vc.SetVsock(uint32(config.VSockContextID), uint32(config.StreamPort))
				}
			}

			ic.Config = reg.Config
		}

		p := reg.Init(ic)
		if err := initializedPlugins.Add(p); err != nil {
			return nil, fmt.Errorf("could not add plugin result to plugin set: %w", err)
		}

		instance, err := p.Instance()
		if err != nil {
			if plugin.IsSkipPlugin(err) {
				log.G(ctx).WithError(err).WithField("plugin_id", id).Info("skip loading plugin")
				continue
			}

			return nil, fmt.Errorf("failed to load plugin %s: %w", id, err)
		}

		if s, ok := instance.(ttrpcService); ok {
			s.RegisterTTRPC(ts)
		}
	}

	return &service{l: l, server: ts}, nil
}
