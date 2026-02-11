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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"

	systemapi "github.com/containerd/nerdbox/api/services/system/v1"
	"github.com/containerd/nerdbox/internal/vm"
)

func TestSystemInfo(t *testing.T) {
	runWithVM(t, func(t *testing.T, i vm.Instance) {
		client := i.Client()

		ss := systemapi.NewTTRPCSystemClient(client)

		resp, err := ss.Info(t.Context(), nil)
		if err != nil {
			t.Fatal("failed to get system info:", err)
		}
		if resp.Version != "dev" {
			t.Fatalf("unexpected version: %s, expected: dev", resp.Version)
		}
		t.Log("Kernel Version:", resp.KernelVersion)
	})
}

func TestConsoleOutput(t *testing.T) {
	var console bytes.Buffer
	startedAt := time.Now()
	runWithVMOpts(t, []vm.StartOpt{vm.WithConsoleWriter(&console)}, func(t *testing.T, i vm.Instance) {
		bootDuration := time.Since(startedAt)
		t.Logf("VM boot took %s", bootDuration)

		// Give a moment for console output to be flushed through the pipe.
		time.Sleep(500 * time.Millisecond)

		output := console.String()
		t.Logf("Console output (%d bytes):\n%s", len(output), output)

		if len(output) == 0 {
			t.Fatal("expected console output from VM boot, got nothing")
		}

		// Check for a kernel boot marker. We use "module loaded" which appears
		// during driver init, rather than "Linux version" which may be lost if
		// the console pipe connects slightly after boot starts.
		if !strings.Contains(output, "module loaded") {
			t.Errorf("console output does not contain 'module loaded'; got:\n%s", output)
		}
	})
}

func TestBootDiagnostics(t *testing.T) {
	var console bytes.Buffer
	startedAt := time.Now()
	runWithVMOpts(t, []vm.StartOpt{vm.WithConsoleWriter(&console)}, func(t *testing.T, i vm.Instance) {
		bootDuration := time.Since(startedAt)

		// Query system info to confirm TTRPC is working.
		client := i.Client()
		ss := systemapi.NewTTRPCSystemClient(client)
		resp, err := ss.Info(t.Context(), nil)
		if err != nil {
			// On failure, dump whatever console output we captured so far
			// to help diagnose why the VM didn't come up.
			time.Sleep(500 * time.Millisecond)
			t.Fatalf("failed to get system info (boot took %s): %v\nConsole output:\n%s",
				bootDuration, err, console.String())
		}

		ttrpcDuration := time.Since(startedAt)
		t.Logf("Boot timing: VM ready in %s, TTRPC responded in %s", bootDuration, ttrpcDuration)
		t.Logf("Kernel: %s, vminitd: %s", resp.KernelVersion, resp.Version)
	})
}

func TestStreamInitialization(t *testing.T) {
	runWithVM(t, func(t *testing.T, i vm.Instance) {
		conn, err := i.StartStream(t.Context(), "test-stream-1")
		if err != nil {
			if errors.Is(err, errdefs.ErrNotImplemented) {
				t.Skip("streaming not implemented")
			}
			t.Fatal("failed to start stream client:", err)
		}

		if err := conn.Close(); err != nil {
			t.Fatal("failed to close stream connection:", err)
		}

		conn, err = i.StartStream(t.Context(), "test-stream-2")
		if err != nil {
			t.Fatal("failed to start stream client:", err)
		}

		if err := conn.Close(); err != nil {
			t.Fatal("failed to close stream connection:", err)
		}
	})
}

// BenchmarkVMStart measures the time to start a VM instance until
// its TTRPC endpoint is ready to serve requests.
func BenchmarkVMStart(b *testing.B) {
	for _, backend := range vmBackends {
		parentDir := b.TempDir()

		b.Run(backend.name, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()

				stateDir, err := os.MkdirTemp(parentDir, "iter-*")
				if err != nil {
					b.Fatal("failed to create temp dir:", err)
				}
				// Resolve symlinks so the VM sees a canonical path.
				resolvedTd, err := filepath.EvalSymlinks(stateDir)
				if err != nil {
					b.Fatal("failed to resolve temp dir:", err)
				}
				b.Chdir(resolvedTd)

				instance, err := backend.vmm.NewInstance(b.Context(), resolvedTd)
				if err != nil {
					b.Fatal("failed to create VM instance:", err)
				}

				b.StartTimer()
				if err := instance.Start(b.Context()); err != nil {
					b.Fatal("failed to start VM:", err)
				}

				b.StopTimer()

				if err := instance.Shutdown(b.Context()); err != nil {
					b.Fatal("failed to shut down VM:", err)
				}

				b.StartTimer()
			}
		})
	}
}

// BenchmarkVMShutdown measures the time to shut down a running VM.
func BenchmarkVMShutdown(b *testing.B) {
	for _, backend := range vmBackends {
		b.Run(backend.name, func(b *testing.B) {
			parentDir := b.TempDir()

			for b.Loop() {
				b.StopTimer()

				stateDir, err := os.MkdirTemp(parentDir, "iter-*")
				if err != nil {
					b.Fatal("failed to create temp dir:", err)
				}
				resolvedTd, err := filepath.EvalSymlinks(stateDir)
				if err != nil {
					b.Fatal("failed to resolve temp dir:", err)
				}
				b.Chdir(resolvedTd)

				instance, err := backend.vmm.NewInstance(b.Context(), resolvedTd)
				if err != nil {
					b.Fatal("failed to create VM instance:", err)
				}
				if err := instance.Start(b.Context()); err != nil {
					b.Fatal("failed to start VM:", err)
				}

				b.StartTimer()
				if err := instance.Shutdown(b.Context()); err != nil {
					b.Fatal("failed to shut down VM:", err)
				}
			}
		})
	}
}
