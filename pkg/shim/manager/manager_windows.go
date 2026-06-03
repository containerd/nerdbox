//go:build windows

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
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	winio "github.com/Microsoft/go-winio"
	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/log"
	"golang.org/x/sys/windows"
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
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd, nil
}

// shimPipeAddress generates a named pipe address for the shim based on the
// containerd address, namespace, and grouping ID — mirroring the Unix socket
// address derivation in CreateSocketAddress.
func shimPipeAddress(ctx context.Context, containerdAddress, grouping string) (string, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return "", err
	}
	path := filepath.Join(containerdAddress, ns, grouping)
	d := sha256.Sum256([]byte(path))
	return fmt.Sprintf(`\\.\pipe\containerd-shim-%x`, d[:16]), nil
}

func (manager) Start(ctx context.Context, bparams *bootapi.BootstrapParams) (_ *bootapi.BootstrapResult, retErr error) {
	id := bparams.InstanceID
	debug := bparams.LogLevel <= bootapi.LogLevel_LOG_LEVEL_DEBUG

	cmd, err := newCommand(ctx, id, bparams.ContainerdGrpcAddress, bparams.ContainerdTtrpcAddress, debug)
	if err != nil {
		return nil, err
	}
	grouping := id
	spec, err := readSpec()
	if err != nil {
		return nil, err
	}
	for _, group := range groupLabels {
		if groupID, ok := spec.Annotations[group]; ok {
			grouping = groupID
			break
		}
	}

	// Generate a named pipe address for the shim TTRPC socket.
	address, err := shimPipeAddress(ctx, bparams.ContainerdGrpcAddress, grouping)
	if err != nil {
		return nil, err
	}

	// Pass the pipe address to the child shim process via environment variable.
	// The shim's serveListener reads TTRPC_SOCKET to know where to listen.
	cmd.Env = append(cmd.Env, "TTRPC_SOCKET="+address)

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			cmd.Process.Kill()
		}
	}()
	// Capture the shim exit error so we can detect an early crash while
	// waiting for the pipe. The channel is buffered so the goroutine never
	// blocks even if we return before reading from it.
	shimExit := make(chan error, 1)
	go func() {
		shimExit <- cmd.Wait()
	}()

	if err = shim.WritePidFile(filepath.Join(bundlePath(ctx), "shim.pid"), cmd.Process.Pid); err != nil {
		return nil, err
	}

	// Wait for the child shim to create the TTRPC named pipe.
	// On Unix, the socket is pre-created via fd passing and exists before
	// the child starts. On Windows, the child creates the pipe after startup,
	// so we must wait for it before returning the address to containerd.
	if err := waitForShimPipe(ctx, address, shimExit,
		shimPipeReadyTimeout,
		shimPipeDialPerAttempt,
		shimPipeRetryDelay,
	); err != nil {
		return nil, err
	}
	return &bootapi.BootstrapResult{
		Version:  3,
		Address:  address,
		Protocol: "ttrpc",
	}, nil
}

const (
	shimPipeReadyTimeout   = 10 * time.Second
	shimPipeDialPerAttempt = 1 * time.Second
	shimPipeRetryDelay     = 10 * time.Millisecond
)

// waitForShimPipe polls a named pipe address with a short per-attempt DialPipe timeout
// until the pipe is reachable, the caller's context is done, the shim signals it has stopped,
// or readyTimeout elapses — whichever comes first.
//
// A short per-attempt timeout prevents a single DialPipe from consuming the
// whole budget when the pipe exists but the shim goroutine has not yet called
// Accept(). Errors that indicate the pipe is not yet ready (not-exist, per-attempt timeout, busy)
// are retried; any other error is fatal.
func waitForShimPipe(ctx context.Context, address string, shimExit <-chan error, readyTimeout, perAttempt, retryDelay time.Duration) error {
	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()

	var (
		dialTimeout time.Duration = perAttempt
		backoff     time.Duration = 0
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case exitErr := <-shimExit:
			// If the shim exited before creating the pipe, report its exit
			// error immediately rather than continuing to poll until timeout.
			if exitErr == nil {
				exitErr = errors.New("exit code 0")
			}
			return fmt.Errorf("shim exited before creating pipe: %w", exitErr)
		case <-timer.C:
			return fmt.Errorf("timed out waiting for shim pipe %s", address)
		case <-time.After(backoff):
			conn, err := winio.DialPipe(address, &dialTimeout)
			if err == nil {
				conn.Close()
				return nil
			}

			// ERROR_PIPE_BUSY is handled internally by go-winio's tryDialPipe
			// loop and surfaces as winio.ErrTimeout once the per-attempt timeout
			// deadline fires; the explicit ERROR_PIPE_BUSY branch is a guard.
			retryable := os.IsNotExist(err) ||
				errors.Is(err, winio.ErrTimeout) ||
				errors.Is(err, windows.ERROR_PIPE_BUSY)
			if !retryable {
				return fmt.Errorf("waiting for shim pipe %s: %w", address, err)
			}

			log.G(ctx).WithError(err).Debug("shim pipe not ready; retry with backoff")

			// Retry with backoff to avoid busy looping when the pipe is not ready.
			backoff = retryDelay
		}
	}
}

// bundlePath extracts the bundle path from the context. The shim framework
// stores it as shim.Opts{BundlePath: ...} via the -bundle flag.
func bundlePath(ctx context.Context) string {
	if o, ok := ctx.Value(shim.OptsKey{}).(shim.Opts); ok {
		return o.BundlePath
	}
	return ""
}

// removeRootfs removes the rootfs directory from the bundle so that
// containerd's bundle cleanup doesn't attempt a bind filter unmount.
// On Windows, Unmount calls bindfilter.RemoveFileBinding which fails with
// ERROR_ACCESS_DENIED on directories that were never bind filter mounts
// (nerdbox uses VM-based virtio block devices instead). Removing the
// directory makes UnmountAll a no-op.
func removeRootfs(ctx context.Context) {
	if bp := bundlePath(ctx); bp != "" {
		os.RemoveAll(filepath.Join(bp, "rootfs"))
	}
}

func (manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	p, err := os.ReadFile(filepath.Join(bundlePath(ctx), "shim.pid"))
	if err != nil {
		if os.IsNotExist(err) {
			// The shim already exited and cleaned up its pid file.
			removeRootfs(ctx)
			return shim.StopStatus{
				ExitedAt:   time.Now(),
				ExitStatus: 128 + 9,
			}, nil
		}
		return shim.StopStatus{}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(p)))
	if err != nil {
		return shim.StopStatus{}, err
	}

	// Open the shim process with the rights needed to terminate it, wait for
	// it to exit, and read its exit code. If OpenProcess fails with
	// ERROR_INVALID_PARAMETER the PID is no longer in the process table —
	// the shim has already exited.
	h, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false,
		uint32(pid),
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			// Process already gone.
			return shim.StopStatus{
				ExitedAt:   time.Now(),
				ExitStatus: 128 + 9,
				Pid:        pid,
			}, nil
		}
		return shim.StopStatus{}, fmt.Errorf("open shim process: %w", err)
	}
	defer windows.CloseHandle(h)

	// Terminate the shim. ERROR_ACCESS_DENIED is returned when the process
	// has already exited but the handle is still open; WaitForSingleObject
	// below will return immediately in that case.
	if err := windows.TerminateProcess(h, uint32(128+9)); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return shim.StopStatus{}, fmt.Errorf("terminate shim process: %w", err)
	}

	// Block until the process has fully exited. There is no timeout: the
	// shim is the only target and TerminateProcess is unconditional, so
	// WaitForSingleObject will always complete.
	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		return shim.StopStatus{}, fmt.Errorf("wait for shim process: %w", err)
	}

	removeRootfs(ctx)

	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + 9,
		Pid:        pid,
	}, nil
}
