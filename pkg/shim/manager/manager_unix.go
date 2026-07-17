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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
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

func (manager) Start(ctx context.Context, bparams *bootapi.BootstrapParams) (_ *bootapi.BootstrapResult, retErr error) {
	id := bparams.InstanceID
	debug := bparams.LogLevel <= bootapi.LogLevel_LOG_LEVEL_DEBUG

	cmd, err := newCommand(ctx, id, bparams.ContainerdGrpcAddress, bparams.ContainerdTtrpcAddress, debug)
	if err != nil {
		return nil, err
	}
	grouping := id
	sp, err := readSpec()
	if err != nil {
		// The sandbox bundle has no config.json when containerd's shim
		// sandboxer creates a sandbox via CRI: core/runtime/v2.NewBundle
		// only writes config.json when the sandbox.Sandbox passed to
		// CreateSandbox carries a non-nil Spec, which CRI's RunPodSandbox
		// never sets before this Start hook runs (the real OCI spec only
		// arrives later, over the Bundle TTRPC service). Grouping by
		// annotation is an optional convenience (falls back to the
		// instance ID, already the default above), not something Start
		// should fail over — only a genuine read error (not "missing
		// file") is fatal.
		if !os.IsNotExist(err) {
			return nil, err
		}
		sp = &spec{}
	}
	for _, group := range groupLabels {
		if groupID, ok := sp.Annotations[group]; ok {
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
	socketDir := bparams.GetSocketDir()
	if socketDir == "" {
		socketDir = filepath.Join(defaults.DefaultStateDir, "s")
	}
	s, err := newShimSocket(ctx, socketDir, bparams.ContainerdGrpcAddress, grouping, false)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return &bootapi.BootstrapResult{
				Version:  3,
				Address:  s.addr,
				Protocol: "ttrpc",
			}, nil
		}
		return nil, err
	}
	sockets = append(sockets, s)
	cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)

	if debug {
		s, err = newShimSocket(ctx, socketDir, bparams.ContainerdGrpcAddress, grouping, true)
		if err != nil {
			return nil, err
		}
		sockets = append(sockets, s)
		cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)
	}

	// Create a temp file in the same directory as shim.pid and acquire an
	// exclusive flock on it.  The fd is handed to the shim via ExtraFiles so
	// the OS keeps the lock alive for exactly the shim's lifetime; when the
	// shim exits every fd referring to that file description is closed and the
	// lock is automatically released.
	pidPath, err := filepath.Abs("shim.pid")
	if err != nil {
		return nil, fmt.Errorf("shim.pid abs path: %w", err)
	}
	pidTmpFile, err := os.CreateTemp(filepath.Dir(pidPath), ".shim.pid.")
	if err != nil {
		return nil, fmt.Errorf("create pid temp file: %w", err)
	}
	pidTmpName := pidTmpFile.Name()
	defer func() {
		if retErr != nil {
			pidTmpFile.Close()
			os.Remove(pidTmpName)
		}
	}()
	if err := syscall.Flock(int(pidTmpFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("acquire flock on pid file: %w", err)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, pidTmpFile)

	userns := cloneMntNs(ctx, cmd)

	if startErr := cmd.Start(); startErr != nil {
		if !userns {
			return nil, startErr
		}
		// clone(CLONE_NEWUSER) can fail for reasons not covered by the
		// proactive AppArmor check — e.g. seccomp filters, LSM policies,
		// or EACCES from the child's capability recomputation when
		// inherited socket fds cross the user namespace boundary after
		// exec. Retry without namespace isolation rather than failing
		// the container start.
		//
		// Note: we cannot log here — during "start" the logger output
		// goes to stderr which containerd captures as part of the
		// bootstrap response (CombinedOutput), corrupting the JSON.
		cmd, err = newCommand(ctx, id, bparams.ContainerdGrpcAddress, bparams.ContainerdTtrpcAddress, debug)
		if err != nil {
			return nil, err
		}
		for _, s := range sockets {
			cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, pidTmpFile)
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("retry without userns failed: %w (original error: %v)", err, startErr)
		}
	}

	defer func() {
		if retErr != nil {
			cmd.Process.Kill()
		}
	}()
	// make sure to wait after start
	go cmd.Wait()

	// TODO: Consider runtime options to supporting adding the shim into a cgroup

	if err := shim.AdjustOOMScore(cmd.Process.Pid); err != nil {
		return nil, fmt.Errorf("failed to adjust OOM score for shim: %w", err)
	}

	// Write the PID into the temp file, sync it to disk, close the parent's
	// copy of the fd (the shim child keeps its inherited copy and therefore
	// holds the flock), then atomically rename to the final "shim.pid" path.
	if _, err := fmt.Fprintf(pidTmpFile, "%d", cmd.Process.Pid); err != nil {
		return nil, fmt.Errorf("write pid: %w", err)
	}
	if err := pidTmpFile.Sync(); err != nil {
		return nil, fmt.Errorf("sync pid file: %w", err)
	}
	if err := pidTmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close pid file: %w", err)
	}
	if err := os.Rename(pidTmpName, pidPath); err != nil {
		return nil, fmt.Errorf("rename pid file: %w", err)
	}

	return &bootapi.BootstrapResult{
		Version:  3,
		Address:  sockets[0].addr,
		Protocol: "ttrpc",
	}, nil
}

func (manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	pid, err := waitForShimPidLock()
	if err != nil {
		return shim.StopStatus{}, err
	}
	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + int(unix.SIGKILL),
		Pid:        pid,
	}, nil
}

// waitForShimPidLock opens shim.pid, reads the shim PID, and blocks until the
// shim process releases the exclusive flock it holds on that file (i.e. until
// the shim exits). It returns the PID on success.
//
// The shim is the only process that acquires the lock, so after SIGKILL it will
// inevitably exit and release it. Blocking unconditionally — rather than racing
// against a timeout — guarantees the caller does not return while the shim is
// still alive.
func waitForShimPidLock() (int, error) {
	f, err := os.Open("shim.pid")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	p, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(p)))
	if err != nil {
		return 0, err
	}

	// Try a non-blocking acquire first. If it succeeds the shim has already
	// exited and the lock is free.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return pid, nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return 0, fmt.Errorf("flock shim.pid: %w", err)
	}

	// Lock is held — shim is still running. Kill it and block until the OS
	// releases the lock on shim exit. There is no timeout: the shim cannot
	// hold the lock after it dies, and returning early would leave it alive.
	if kerr := unix.Kill(pid, unix.SIGKILL); kerr != nil && !errors.Is(kerr, unix.ESRCH) {
		return 0, fmt.Errorf("kill shim: %w", kerr)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, fmt.Errorf("flock shim.pid (wait): %w", err)
	}
	return pid, nil
}
