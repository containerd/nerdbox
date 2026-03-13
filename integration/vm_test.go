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
	"errors"
	"os"
	"path/filepath"
	"testing"

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

// BenchmarkVMStart measures the time to create and start a VM instance until
// its TTRPC endpoint is ready to serve requests.
func BenchmarkVMStart(b *testing.B) {
	for _, backend := range vmBackends {
		b.Run(backend.name, func(b *testing.B) {
			for b.Loop() {
				td := b.TempDir()
				// Resolve symlinks so the VM sees a canonical path.
				resolvedTd, err := filepath.EvalSymlinks(td)
				if err != nil {
					b.Fatal("failed to resolve temp dir:", err)
				}
				if err := os.Chdir(resolvedTd); err != nil {
					b.Fatal("failed to chdir:", err)
				}

				instance, err := backend.vmm.NewInstance(b.Context(), resolvedTd)
				if err != nil {
					b.Fatal("failed to create VM instance:", err)
				}

				b.StartTimer()
				if err := instance.Start(b.Context()); err != nil {
					b.Fatal("failed to start VM:", err)
				}
				b.StopTimer()

				instance.Shutdown(b.Context())
			}
		})
	}
}

// BenchmarkVMShutdown measures the time to shut down a running VM.
func BenchmarkVMShutdown(b *testing.B) {
	for _, backend := range vmBackends {
		b.Run(backend.name, func(b *testing.B) {
			for b.Loop() {
				td := b.TempDir()
				resolvedTd, err := filepath.EvalSymlinks(td)
				if err != nil {
					b.Fatal("failed to resolve temp dir:", err)
				}
				if err := os.Chdir(resolvedTd); err != nil {
					b.Fatal("failed to chdir:", err)
				}

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
				b.StopTimer()
			}
		})
	}
}

// BenchmarkVMStartStop measures the full round-trip: create instance, start,
// and shut down — the end-to-end cost of a single VM lifecycle.
func BenchmarkVMStartStop(b *testing.B) {
	for _, backend := range vmBackends {
		b.Run(backend.name, func(b *testing.B) {
			for b.Loop() {
				td := b.TempDir()
				resolvedTd, err := filepath.EvalSymlinks(td)
				if err != nil {
					b.Fatal("failed to resolve temp dir:", err)
				}
				if err := os.Chdir(resolvedTd); err != nil {
					b.Fatal("failed to chdir:", err)
				}

				instance, err := backend.vmm.NewInstance(b.Context(), resolvedTd)
				if err != nil {
					b.Fatal("failed to create VM instance:", err)
				}
				if err := instance.Start(b.Context()); err != nil {
					b.Fatal("failed to start VM:", err)
				}
				if err := instance.Shutdown(b.Context()); err != nil {
					b.Fatal("failed to shut down VM:", err)
				}
			}
		})
	}
}
