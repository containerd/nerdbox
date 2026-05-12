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
	"io"
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
	"github.com/containerd/nerdbox/internal/vminit/vmnetworking"
	"github.com/containerd/nerdbox/plugins"
)

// logLevel controls the slog handler level for vminitd.
var logLevel = &slog.LevelVar{}

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

	log.G(ctx).WithField("t", time.Since(t1)).Debug("initialized vminitd")

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
	if err := switchRoot(ctx); err != nil {
		return fmt.Errorf("failed to switch root: %w", err)
	}
	log.G(ctx).WithField("elapsed", time.Since(t)).Debug("switch root completed")

	dhcpRenewer, dhcpReleaser, err := vmnetworking.SetupVM(ctx, config.Networks, config.Debug)
	if err != nil {
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

// switchRoot builds a new tmpfs root filesystem, mounts all pseudo-filesystems
// directly into it, copies crun, configures cgroups, then switches root using
// MS_MOVE + chroot. By mounting directly into the new root from the start no
// mount moves are required — the pseudo-filesystems are never on the old root.
// This detaches the initramfs so its pages can be reclaimed and gives
// containers a distinct filesystem so pivot_root works correctly.
func switchRoot(ctx context.Context) error {
	const root = "/newroot"

	if err := os.Mkdir(root, 0755); err != nil {
		return fmt.Errorf("create new root dir: %w", err)
	}
	if err := unix.Mount("tmpfs", root, "tmpfs", 0, "size=128m,mode=0755"); err != nil {
		return fmt.Errorf("mount tmpfs at new root: %w", err)
	}

	// Create mount-point directories and /etc directly. We just mounted a
	// fresh tmpfs so none of these exist yet — os.Mkdir is sufficient and
	// avoids the stat walk that os.MkdirAll does per path. /sys/fs/cgroup
	// is not pre-created here: sysfs provides that directory via the kernel's
	// cgroup2 kobject registered at boot, so it exists once sysfs is mounted.
	for _, dir := range []string{
		root + "/sbin",
		root + "/proc",
		root + "/sys",
		root + "/dev",
		root + "/run",
		root + "/tmp",
		root + "/etc",
	} {
		if err := os.Mkdir(dir, 0755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}

	tCopy := time.Now()
	if err := copyCrun("/sbin/crun", root+"/sbin/crun"); err != nil {
		return fmt.Errorf("copy crun to new root: %w", err)
	}
	// Remove the initramfs copy of crun to reclaim its pages before the
	// root switch. The init binary is intentionally left — deleting it
	// would only make it inaccessible to userspace, not free memory, since
	// its pages are already pinned by the running process.
	if err := os.Remove("/sbin/crun"); err != nil {
		return fmt.Errorf("remove old crun: %w", err)
	}
	log.G(ctx).WithField("elapsed", time.Since(tCopy)).Debug("crun copied")

	// Mount pseudo-filesystems directly into the new root — no moves needed.
	// sysfs must be mounted before cgroup2: the kernel's cgroup2 kobject
	// populates /sys/fs/cgroup inside sysfs, which serves as the cgroup2
	// mountpoint.
	if err := mount.All([]mount.Mount{
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
			Options: []string{"nosuid", "noexec", "nodev", "size=64m", "mode=0755"},
		},
		{
			Type:    "devtmpfs",
			Source:  "devtmpfs",
			Target:  "/dev",
			Options: []string{"nosuid", "noexec"},
		},
	}, root); err != nil {
		return fmt.Errorf("mount pseudo-filesystems: %w", err)
	}

	// Configure cgroup v2 controllers while the path is still explicit.
	if err := os.WriteFile(root+"/sys/fs/cgroup/cgroup.subtree_control",
		[]byte("+cpu +cpuset +io +memory +pids"), 0644); err != nil {
		return fmt.Errorf("setup cgroup control: %w", err)
	}

	// Make the root mount private so MS_MOVE does not propagate to any peer
	// groups before we replace it.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make root mounts private: %w", err)
	}

	// Atomically replace the root mount with the new tmpfs, then update the
	// process root via chroot. MS_MOVE + chroot is the busybox switch_root
	// approach — no pivot_root required, works on any kernel version.
	if err := os.Chdir(root); err != nil {
		return fmt.Errorf("chdir to new root: %w", err)
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move new root to /: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot to new root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir to /: %w", err)
	}

	return nil
}

func copyCrun(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
		ttrpc.WithUnaryServerInterceptor(otelttrpc.UnaryServerInterceptor()),
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
				log.G(ctx).WithFields(log.Fields{"error": err, "plugin_id": id}).Info("skip loading plugin")
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
