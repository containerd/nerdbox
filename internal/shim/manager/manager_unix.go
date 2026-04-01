//go:build !windows

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

package manager

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

func newCommand(ctx context.Context, id, containerdAddress, containerdTTRPCAddress string, debug bool) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-namespace", ns,
		"-id", id,
		"-address", containerdAddress,
	}
	if debug {
		args = append(args, "-debug")
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	cmd.Env = append(cmd.Env, "OTEL_SERVICE_NAME=containerd-shim-"+id)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	return cmd, nil
}

type shimSocket struct {
	addr string
	s    *net.UnixListener
	f    *os.File
}

func (s *shimSocket) Close() {
	if s.s != nil {
		s.s.Close()
	}
	if s.f != nil {
		s.f.Close()
	}
	_ = shim.RemoveSocket(s.addr)
}

func newShimSocket(ctx context.Context, root, path, id string, debug bool) (*shimSocket, error) {
	address, err := shim.CreateSocketAddress(ctx, root, path, id, debug)
	if err != nil {
		return nil, err
	}

	// Workaround: shim.NewSocket expects the parent directory to exist.
	// Ensure the socket directory exists before creating the socket.
	// TODO: Remove after https://github.com/containerd/containerd/pull/12960
	addrParentDir := filepath.Dir(strings.TrimPrefix(address, "unix://"))
	if err := os.MkdirAll(addrParentDir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	socket, err := shim.NewSocket(address)
	if err != nil {
		// the only time where this would happen is if there is a bug and the socket
		// was not cleaned up in the cleanup method of the shim or we are using the
		// grouping functionality where the new process should be run with the same
		// shim as an existing container
		if !shim.SocketEaddrinuse(err) {
			return nil, fmt.Errorf("create new shim socket: %w", err)
		}
		if !debug && shim.CanConnect(address) {
			return &shimSocket{addr: address}, errdefs.ErrAlreadyExists
		}
		if err := shim.RemoveSocket(address); err != nil {
			return nil, fmt.Errorf("remove pre-existing socket: %w", err)
		}
		if socket, err = shim.NewSocket(address); err != nil {
			return nil, fmt.Errorf("try create new shim socket 2x: %w", err)
		}
	}
	s := &shimSocket{
		addr: address,
		s:    socket,
	}
	f, err := socket.File()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.f = f
	return s, nil
}

func (manager) Start(ctx context.Context, id string, opts shim.StartOpts) (_ shim.BootstrapParams, retErr error) {
	var params shim.BootstrapParams
	params.Version = 3
	params.Protocol = "ttrpc"

	cmd, err := newCommand(ctx, id, opts.Address, opts.TTRPCAddress, opts.Debug)
	if err != nil {
		return params, err
	}
	grouping := id
	spec, err := readSpec()
	if err != nil {
		return params, err
	}
	for _, group := range groupLabels {
		if groupID, ok := spec.Annotations[group]; ok {
			grouping = groupID
			break
		}
	}

	var sockets []*shimSocket
	defer func() {
		if retErr != nil {
			for _, s := range sockets {
				s.Close()
			}
		}
	}()
	socketDir := opts.SocketDir
	if socketDir == "" {
		socketDir = filepath.Join(defaults.DefaultStateDir, "s")
	}
	s, err := newShimSocket(ctx, socketDir, opts.Address, grouping, false)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			params.Address = s.addr
			return params, nil
		}
		return params, err
	}
	sockets = append(sockets, s)
	cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)

	if opts.Debug {
		s, err = newShimSocket(ctx, socketDir, opts.Address, grouping, true)
		if err != nil {
			return params, err
		}
		sockets = append(sockets, s)
		cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)
	}

	cloneMntNs(ctx, cmd)

	if err := cmd.Start(); err != nil {
		return params, err
	}

	defer func() {
		if retErr != nil {
			cmd.Process.Kill()
		}
	}()
	// make sure to wait after start
	go cmd.Wait()

	/*
		if opts, err := shim.ReadRuntimeOptions[*options.Options](os.Stdin); err == nil {
				if opts.ShimCgroup != "" {
					if cgroups.Mode() == cgroups.Unified {
						cg, err := cgroupsv2.Load(opts.ShimCgroup)
						if err != nil {
							return params, fmt.Errorf("failed to load cgroup %s: %w", opts.ShimCgroup, err)
						}
						if err := cg.AddProc(uint64(cmd.Process.Pid)); err != nil {
							return params, fmt.Errorf("failed to join cgroup %s: %w", opts.ShimCgroup, err)
						}
					} else {
						cg, err := cgroup1.Load(cgroup1.StaticPath(opts.ShimCgroup))
						if err != nil {
							return params, fmt.Errorf("failed to load cgroup %s: %w", opts.ShimCgroup, err)
						}
						if err := cg.AddProc(uint64(cmd.Process.Pid)); err != nil {
							return params, fmt.Errorf("failed to join cgroup %s: %w", opts.ShimCgroup, err)
						}
					}
				}
		}

		if err := shim.AdjustOOMScore(cmd.Process.Pid); err != nil {
			return params, fmt.Errorf("failed to adjust OOM score for shim: %w", err)
		}
	*/

	if err = shim.WritePidFile("shim.pid", cmd.Process.Pid); err != nil {
		return params, err
	}

	params.Address = sockets[0].addr
	return params, nil
}

func (manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	p, err := os.ReadFile("shim.pid")
	if err != nil {
		return shim.StopStatus{}, err
	}
	pid, err := strconv.Atoi(string(p))
	if err != nil {
		return shim.StopStatus{}, err
	}
	/*
		cwd, err := os.Getwd()
		if err != nil {
			return shim.StopStatus{}, err
		}

		path := filepath.Join(filepath.Dir(cwd), id)
		ns, err := namespaces.NamespaceRequired(ctx)
		if err != nil {
			return shim.StopStatus{}, err
		}
		runtime, err := runc.ReadRuntime(path)
		if err != nil && !os.IsNotExist(err) {
			return shim.StopStatus{}, err
		}
		opts, err := runc.ReadOptions(path)
		if err != nil {
			return shim.StopStatus{}, err
		}
		root := process.RuncRoot
		if opts != nil && opts.Root != "" {
			root = opts.Root
		}

		r := process.NewRunc(root, path, ns, runtime, false)
		if err := r.Delete(ctx, id, &runcC.DeleteOpts{
			Force: true,
		}); err != nil {
			log.G(ctx).WithError(err).Warn("failed to remove runc container")
		}
		if err := mount.UnmountRecursive(filepath.Join(path, "rootfs"), 0); err != nil {
			log.G(ctx).WithError(err).Warn("failed to cleanup rootfs mount")
		}
		pid, err := runcC.ReadPidFile(filepath.Join(path, process.InitPidFile))
		if err != nil {
			log.G(ctx).WithError(err).Warn("failed to read init pid file")
		}
	*/
	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + int(unix.SIGKILL),
		Pid:        pid,
	}, nil
}
