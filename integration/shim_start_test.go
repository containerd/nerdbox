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
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const shimBinary = "containerd-shim-nerdbox-v1"

// TestShimStart exercises the shim manager's Start() code path by invoking
// the real shim binary with the "start" subcommand. This is the same
// invocation containerd uses to launch a shim.
func TestShimStart(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	shimID := strings.ReplaceAll(t.Name(), "/", "-")
	cmd := exec.CommandContext(ctx, shimPath,
		"-namespace", "test",
		"-id", shimID,
		"-address", filepath.Join(socketDir, "containerd.sock"),
		"start",
	)
	cmd.Dir = bundleDir
	cmd.Env = append(os.Environ(),
		// SHIM_SOCKET_DIR is read by the containerd shim framework (our
		// fork) to populate StartOpts.SocketDir so the shim creates its
		// sockets in a writable directory instead of /run/containerd/s.
		"SHIM_SOCKET_DIR="+socketDir,
	)

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

	var params struct {
		Version  int    `json:"version"`
		Address  string `json:"address"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(out, &params); err != nil {
		t.Fatalf("failed to parse shim output: %v\nraw: %s", err, out)
	}
	if params.Address == "" {
		t.Fatal("shim returned empty address")
	}
	t.Logf("shim started: version=%d protocol=%s address=%s", params.Version, params.Protocol, params.Address)
}
