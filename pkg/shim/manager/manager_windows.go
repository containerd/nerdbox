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
	"math/rand/v2"
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

	"github.com/containerd/nerdbox/internal/erofs"
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
	sp, err := readSpec()
	if err != nil {
		// See the identical comment in manager_unix.go's Start: the sandbox
		// bundle has no config.json when containerd's shim sandboxer
		// creates a sandbox via CRI, and grouping-by-annotation is an
		// optional convenience, not something Start should fail over.
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

	// Stop waits for the shim to exit and then clears the bundle. The sum of
	// the two budgets must leave slack inside containerd's
	// "io.containerd.timeout.shim.cleanup" (5s by default), which bounds the
	// whole `shim delete` binary call: if containerd kills us instead, the
	// deferred bundle cleanup never runs at all.
	shimExitWaitTimeout = 2 * time.Second
	bundleRemoveWindow  = 1 * time.Second

	shimExitPollInterval   = 50 * time.Millisecond
	bundleRemoveRetryDelay = 200 * time.Millisecond
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

	shimExitErr := func(exitErr error) error {
		// If the shim exited before creating the pipe, report its exit
		// error immediately rather than continuing to poll until timeout.
		if exitErr == nil {
			exitErr = errors.New("exit code 0")
		}
		return fmt.Errorf("shim exited before creating pipe: %w", exitErr)
	}

	// checkCancel does a non-blocking probe of the three cancel cases.
	// Returns a non-nil error if a cancel/exit/timeout case is pending,
	// nil otherwise. Running this at the top of each iteration gives the
	// cancel cases precedence over the DialPipe attempt that follows —
	// Go's select picks randomly among ready cases, so without this guard
	// a just-fired cancel could lose to a backoff timer that fired in the
	// same tick.
	checkCancel := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case exitErr := <-shimExit:
			return shimExitErr(exitErr)
		case <-timer.C:
			return fmt.Errorf("timed out waiting for shim pipe %s", address)
		default:
			return nil
		}
	}

	// sleepCancel waits up to backoff, returning early with a cancel error
	// if any cancel case fires during the wait.
	sleepCancel := func(backoff time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case exitErr := <-shimExit:
			return shimExitErr(exitErr)
		case <-timer.C:
			return fmt.Errorf("timed out waiting for shim pipe %s", address)
		case <-time.After(backoff):
			return nil
		}
	}

	for {
		if err := checkCancel(); err != nil {
			return err
		}

		dialTimeout := perAttempt
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

		log.G(ctx).WithError(err).Debug("shim pipe not ready; backing off before retry")

		// Backoff + jitter (up to 100% of base delay)
		backoff := retryDelay + time.Duration(rand.Int64N(int64(retryDelay)))
		if err := sleepCancel(backoff); err != nil {
			return err
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

// removeBundleArtifacts removes everything the shim itself put in the bundle
// directory, leaving containerd's own bundle cleanup nothing to trip over. Two
// Windows failure modes make it necessary: Unmount calls
// bindfilter.RemoveFileBinding, which fails with ERROR_ACCESS_DENIED on a rootfs
// that was never a bind filter mount (nerdbox uses virtio block devices
// instead), and a VMDK extent still mapped by the VM cannot be unlinked at all —
// see [erofs.IsBundleArtifact].
func removeBundleArtifacts(ctx context.Context) {
	bp := bundlePath(ctx)
	if bp == "" {
		return
	}

	targets := []string{filepath.Join(bp, "rootfs")}
	entries, err := os.ReadDir(bp)
	if err != nil {
		log.G(ctx).WithError(err).WithField("bundle", bp).
			Error("failed to list bundle directory; shim-written artifacts may be left behind")
	}
	for _, entry := range entries {
		if erofs.IsBundleArtifact(entry.Name()) {
			targets = append(targets, filepath.Join(bp, entry.Name()))
		}
	}

	// A shim being terminated can hold its mappings for a moment after
	// TerminateProcess returns, so retry — but over the whole remaining set
	// under one deadline. Retrying per target would multiply by the target
	// count and could push this call past containerd's cleanup timeout, which
	// is the very failure being fixed.
	deadline := time.Now().Add(bundleRemoveWindow)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	var failures map[string]error
	for {
		var remaining []string
		failures = make(map[string]error, len(targets))
		for _, target := range targets {
			if err := os.RemoveAll(target); err != nil {
				failures[target] = err
				remaining = append(remaining, target)
			}
		}
		targets = remaining
		if len(targets) == 0 || time.Until(deadline) <= bundleRemoveRetryDelay {
			break
		}
		time.Sleep(bundleRemoveRetryDelay)
	}

	// Name the survivors: this is the file that will wedge subsequent starts.
	for _, target := range targets {
		log.G(ctx).WithError(failures[target]).WithField("path", target).
			Error("failed to remove bundle artifact; containerd bundle cleanup and subsequent starts of this container will fail until it is released")
	}
}

// waitForProcessExit blocks until the process handle h is signalled, ctx is
// done, or timeout elapses — whichever comes first. It reports whether the
// process exited.
//
// The wait is polled rather than a single WaitForSingleObject(INFINITE) so it
// can honour ctx: containerd bounds the `shim delete` binary call, and being
// killed mid-call would skip our deferred bundle cleanup.
func waitForProcessExit(ctx context.Context, h windows.Handle, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		if err := ctx.Err(); err != nil {
			return false
		}

		wait := shimExitPollInterval
		if remaining < wait {
			wait = remaining
		}
		// WaitForSingleObject returns WAIT_OBJECT_0 (0) when the process has
		// exited and WAIT_TIMEOUT when the interval elapsed first.
		event, err := windows.WaitForSingleObject(h, uint32(wait.Milliseconds()))
		if err != nil {
			return false
		}
		if event == uint32(windows.WAIT_OBJECT_0) {
			return true
		}
	}
}

func (manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	// must run on all exits (including when the process is already gone)
	// to ensure containerd's bundle cleanup is successful. See
	// [removeBundleArtifacts] for more details. Every wait below is bounded so
	// that this defer actually gets to run — containerd kills the `shim delete`
	// call once "io.containerd.timeout.shim.cleanup" expires, and a killed
	// process runs no defers.
	defer removeBundleArtifacts(ctx)

	p, err := os.ReadFile(filepath.Join(bundlePath(ctx), "shim.pid"))
	if err != nil {
		if os.IsNotExist(err) {
			// The shim already exited and cleaned up its pid file.
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

	// Wait for the process to fully exit, but only for a bounded period.
	// TerminateProcess is not instantaneous: a VM-backed shim can have threads
	// parked in kernel-mode hypervisor or memory-mapping calls, and the process
	// does not die until those return. containerd bounds this whole binary call
	// by "io.containerd.timeout.shim.cleanup" (5s), so waiting forever means
	// being killed and skipping the bundle cleanup deferred above — which is
	// what leaves a locked VMDK extent behind and wedges the container id.
	//
	// Report success either way: containerd proceeds to delete the bundle
	// regardless of what we return here, so the useful thing is to finish under
	// our own control with the cleanup done, and to say loudly when the shim
	// outlived us.
	if !waitForProcessExit(ctx, h, shimExitWaitTimeout) {
		log.G(ctx).WithFields(log.Fields{
			"pid":     pid,
			"id":      id,
			"timeout": shimExitWaitTimeout,
		}).Error("shim process did not exit before the wait budget expired; it may still hold bundle files")
	}

	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + 9,
		Pid:        pid,
	}, nil
}
