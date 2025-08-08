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

package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/otelttrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	_ "github.com/containerd/containerd/v2/plugins/events"

	_ "github.com/dmcgowan/nerdbox/plugins/services/system"
	_ "github.com/dmcgowan/nerdbox/plugins/vminit/task"
)

func main() {
	t1 := time.Now()
	var (
		config ServiceConfig
		dev    = flag.Bool("dev", false, "Development mode with graceful exit")
		debug  = flag.Bool("debug", true, "Debug log level")
	)
	flag.IntVar(&config.VSockPort, "vsock-port", 1024, "vsock port to listen on")
	flag.IntVar(&config.VSockContextID, "vsock-cid", 0, "vsock context ID for vsock listen")

	args := os.Args[1:]
	// Strip "tsi_hijack" added by libkrun
	if len(args) > 0 && args[0] == "tsi_hijack" {
		args = args[1:]
	}
	flag.CommandLine.Parse(args)
	flag.Parse()

	c, err := os.OpenFile("/dev/console", os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open /dev/console: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()
	log.L.Logger.SetOutput(c)

	if *dev || *debug {
		log.SetLevel("debug")
	}

	ctx := context.Background()

	log.G(ctx).WithField("args", args).WithField("env", os.Environ()).Debug("starting vminitd")

	defer func() {
		if err != nil {
			log.G(ctx).WithError(err).Error("exiting with error")
		} else if p := recover(); p != nil {
			log.G(ctx).WithField("panic", p).Error("recovered from panic")
		} else {
			log.G(ctx).Debug("exiting cleanly")
		}

		if !*dev {
			// Exit method is based on VM manager
			// For krun, just os.Exit(0) is sufficient

			log.G(ctx).Debug("poweroff")
			//unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
			//os.Exit(1)
			//unix.Kill(1, unix.SIGTERM) // Send SIGTERM to init process
		}
	}()

	if err = systemInit(); err != nil {
		return
	}

	if *debug {
		filepath.Walk("/", func(path string, info os.FileInfo, err error) error {
			if path == "/proc" || path == "/sys" {
				path = fmt.Sprintf("%s (skipping)", path)
				err = filepath.SkipDir
			}

			if info != nil {
				log.G(ctx).WithFields(
					log.Fields{
						"mode": info.Mode(),
						"size": info.Size(),
					}).Debug(path)
			}

			return err
		})

		b, err := os.ReadFile("/proc/cmdline")
		if err != nil {
			return
		}
		log.G(ctx).WithField("cmdline", string(b)).Debug("kernel command line")
	}

	ctx, config.Shutdown = shutdown.WithShutdown(ctx)
	defer config.Shutdown.Shutdown()

	service, err := New(ctx, config)
	if err != nil {
		return
	}

	log.G(ctx).WithField("t", time.Since(t1)).Debug("initialized vminitd")

	err = service.Run(ctx)
}

func systemInit() error {
	if err := systemMounts(); err != nil {
		return err
	}

	if err := unix.Mknod(os.DevNull, uint32(fs.ModeDevice|fs.ModeCharDevice|0666), int(unix.Mkdev(1, 3))); err != nil {
		return err
	}

	return setupCgroupControl()
}

func systemMounts() error {
	return mount.All([]mount.Mount{
		{
			Type:   "proc",
			Source: "proc",
			Target: "/proc",
		},
		{
			Type:   "sysfs",
			Source: "sysfs",
			Target: "/sys",
		},
		{
			Type:   "cgroup2",
			Source: "none",
			Target: "/sys/fs/cgroup",
		},
		{
			Type:   "tmpfs",
			Source: "tmpfs",
			Target: "/run",
		},
		{
			Type:   "tmpfs",
			Source: "tmpsfs",
			Target: "/tmp",
		},
	}, "/")
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

type Runnable interface {
	Run(context.Context) error
}

type ServiceConfig struct {
	VSockContextID int
	VSockPort      int
	Shutdown       shutdown.Service
}

func New(ctx context.Context, config ServiceConfig) (Runnable, error) {
	var (
		initializedPlugins = plugin.NewPluginSet()
		pluginProperties   = map[string]string{}
		disabledPlugins    = map[string]struct{}{}
		// TODO: service config?
	)

	l, err := vsock.ListenContextID(uint32(config.VSockContextID), uint32(config.VSockPort), &vsock.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on vsock port %d with context id %d: %w", config.VSockPort, config.VSockContextID, err)
	}
	ts, err := ttrpc.NewServer(
		ttrpc.WithUnaryServerInterceptor(otelttrpc.UnaryServerInterceptor()),
	)
	if err != nil {
		return nil, err
	}

	registry.Register(&plugin.Registration{
		Type: plugins.InternalPlugin,
		ID:   "shutdown",
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			return config.Shutdown, nil
		},
	})

	// TODO: Allow disabling plugins
	//if len(cfg.DisabledPlugins) > 0 {
	//	for _, p := range cfg.DisabledPlugins {
	//		disabledPlugins[p] = struct{}{}
	//	}
	//}

	for _, reg := range registry.Graph(func(*plugin.Registration) bool { return false }) {
		id := reg.URI()
		if _, ok := disabledPlugins[id]; ok {
			log.G(ctx).WithField("plugin_id", id).Info("plugin is disabled, skipping load")
			continue
		}

		log.G(ctx).WithField("plugin_id", id).Info("loading plugin")

		ic := plugin.NewContext(ctx, initializedPlugins, pluginProperties)

		if reg.Config != nil {
			// TODO: Allow plugin config?
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

	return &service{
		l:      l,
		server: ts,
	}, nil
}

func (s *service) Run(ctx context.Context) error {
	s.server.Serve(ctx, s.l)
	return nil
}

func newTTRPCServer() (*ttrpc.Server, error) {
	return ttrpc.NewServer(
		ttrpc.WithUnaryServerInterceptor(otelttrpc.UnaryServerInterceptor()),
	)
}
