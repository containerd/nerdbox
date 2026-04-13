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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	winio "github.com/Microsoft/go-winio"
	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
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
	// make sure to wait after start
	go cmd.Wait()

	if err = shim.WritePidFile("shim.pid", cmd.Process.Pid); err != nil {
		return nil, err
	}

	// Wait for the child shim to create the TTRPC named pipe.
	// On Unix, the socket is pre-created via fd passing and exists before
	// the child starts. On Windows, the child creates the pipe after startup,
	// so we must wait for it before returning the address to containerd.
	deadline := time.Now().Add(10 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		conn, err := winio.DialPipe(address, nil)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("waiting for shim pipe %s: %w", address, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		return nil, fmt.Errorf("timed out waiting for shim pipe %s", address)
	}

	return &bootapi.BootstrapResult{
		Version:  3,
		Address:  address,
		Protocol: "ttrpc",
	}, nil
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
	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + 9, // 128 + SIGKILL
		Pid:        pid,
	}, nil
}
