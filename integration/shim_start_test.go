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

package integration

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/nerdbox/internal/ttrpcutil"
	"google.golang.org/protobuf/proto"
)

const shimBinary = "containerd-shim-nerdbox-v1"

// startShim launches the shim binary with the "start" subcommand and returns
// the bootstrap result. It registers a cleanup function to kill the
// spawned shim process.
func startShim(t *testing.T) *bootapi.BootstrapResult {
	t.Helper()

	shimPath, err := exec.LookPath(shimBinary)
	if err != nil {
		t.Fatalf("%s not found on PATH: %v", shimBinary, err)
	}

	bundleDir := t.TempDir()
	socketDir := t.TempDir()

	// Minimal OCI config.json — Start() only reads annotations.
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"annotations":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The shim child process expects a "log" FIFO in the bundle directory
	// (used by openLog/fifo.OpenFifoDup2 for redirecting stderr). Create
	// one and drain it in the background so the shim doesn't block.
	logFIFO := filepath.Join(bundleDir, "log")
	if err := syscall.Mkfifo(logFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		f, err := os.Open(logFIFO)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = io.Copy(io.Discard, f)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	shimID := strings.ReplaceAll(t.Name(), "/", "-")
	containerdAddr := filepath.Join(socketDir, "containerd.sock")
	cmd := exec.CommandContext(ctx, shimPath,
		"-namespace", "test",
		"-id", shimID,
		"-address", containerdAddr,
		"start",
	)
	cmd.Dir = bundleDir
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")

	// Pass bootstrap params via stdin using the new bootstrap protocol.
	// The shim framework reads BootstrapParams from stdin to get the
	// socket directory and other configuration.
	bparams := &bootapi.BootstrapParams{
		InstanceID:            shimID,
		Namespace:             "test",
		ContainerdGrpcAddress: containerdAddr,
		SocketDir:             &socketDir,
	}
	bdata, err := proto.Marshal(bparams)
	if err != nil {
		t.Fatalf("failed to marshal bootstrap params: %v", err)
	}
	cmd.Stdin = bytes.NewReader(bdata)

	// Register cleanup before cmd.Output() so that the spawned shim child
	// is killed even if later assertions fail or the test times out.
	t.Cleanup(func() {
		pidData, err := os.ReadFile(filepath.Join(bundleDir, "shim.pid"))
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if err != nil {
			return
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("shim start failed: %v\nstderr: %s", err, stderr)
	}

	var result bootapi.BootstrapResult
	if err := proto.Unmarshal(out, &result); err != nil {
		t.Fatalf("failed to parse shim output: %v\nraw: %x", err, out)
	}
	if result.Address == "" {
		t.Fatal("shim returned empty address")
	}
	return &result
}

// TestShimStart exercises the shim manager's Start() code path by invoking
// the real shim binary with the "start" subcommand. This is the same
// invocation containerd uses to launch a shim.
func TestShimStart(t *testing.T) {
	result := startShim(t)
	t.Logf("shim started: version=%d protocol=%s address=%s", result.Version, result.Protocol, result.Address)
}

// TestShimConnect verifies that the shim's TTRPC server is reachable after
// Start returns. This exercises the same code path containerd uses when it
// dials the shim socket to create a task. A failure here reproduces the
// "failed to create TTRPC connection: dial unix …: connect: no such file or
// directory" error seen in CI.
func TestShimConnect(t *testing.T) {
	result := startShim(t)
	t.Logf("shim started: version=%d protocol=%s address=%s", result.Version, result.Protocol, result.Address)

	socketPath := strings.TrimPrefix(result.Address, "unix://")

	// Poll for the TTRPC server to become ready, same as containerd does
	// after the shim's Start returns.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var conn net.Conn
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for shim TTRPC server at %s", socketPath)
		default:
		}

		if _, err := os.Stat(socketPath); err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		c, err := net.Dial("unix", socketPath)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		c.SetReadDeadline(time.Now().Add(time.Second))
		if err := ttrpcutil.PingTTRPC(c); err != nil {
			c.Close()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		c.SetReadDeadline(time.Time{})
		conn = c
		break
	}
	defer conn.Close()

	t.Logf("TTRPC connection established to shim at %s", socketPath)
}
