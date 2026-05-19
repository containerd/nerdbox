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
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/log/logtest"

	"github.com/containerd/nerdbox/internal/vm/libkrun"
	"github.com/containerd/nerdbox/pkg/logging"
	"github.com/containerd/nerdbox/pkg/vm"
)

// tLogWriter routes slog records to t.Log() so they are suppressed by the
// test framework unless the test fails or -v is used.
type tLogWriter struct {
	mu sync.Mutex
	t  testing.TB
}

func (w *tLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.t == nil {
		// Test has completed; silently discard to avoid "Log after test
		// finished" panics from any background goroutine still draining
		// the console FIFO.
		return len(p), nil
	}
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// disable clears the reference to t so that subsequent Write calls are
// discarded. Call this after vm.Shutdown() in t.Cleanup to ensure no
// background log goroutine can call t.Log() on a completed test.
func (w *tLogWriter) disable() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.t = nil
}

func TestMain(m *testing.M) {
	e, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}
	exeDir := filepath.Dir(e)
	paths := filepath.SplitList(os.Getenv("PATH"))
	for _, p := range []string{
		filepath.Join("..", "_output"),
		".",
	} {
		absPath := filepath.Clean(filepath.Join(exeDir, p))
		// Prepend to slice
		paths = append(paths, "")
		copy(paths[1:], paths)
		paths[0] = absPath
	}
	if err := os.Setenv("PATH", strings.Join(paths, string(filepath.ListSeparator))); err != nil {
		log.Fatalf("Failed to set PATH environment variable: %v", err)
	}

	r := m.Run()

	os.Exit(r)
}

var vmBackends = []struct {
	name string
	vmm  vm.Manager
}{
	{name: "libkrun", vmm: libkrun.NewManager()},
}

func runWithVM(t *testing.T, runTest func(*testing.T, vm.Instance)) {
	runWithVMOpts(t, nil, runTest)
}

func runWithVMOpts(t *testing.T, startOpts []vm.StartOpt, runTest func(*testing.T, vm.Instance)) {
	for _, tc := range vmBackends {
		t.Run(tc.name, func(t *testing.T) {
			// Route all log output through t.Log() so it is suppressed
			// unless the test fails or -v is used.
			//
			// logtest.WithT redirects logrus (log.G(ctx)) to t.Log().
			ctx := logtest.WithT(t.Context(), t)
			// SetBaseHandler redirects slog (ForwardConsoleLogs: kernel
			// kmsg + vminitd JSON) to t.Log() via a TextHandler.
			lw := &tLogWriter{t: t}
			logging.SetBaseHandler(slog.NewTextHandler(
				lw,
				&slog.HandlerOptions{Level: slog.LevelDebug},
			))

			td := t.TempDir()
			t.Chdir(td)
			// Use Getwd to resolve symlinks (e.g., /var -> /private/var on macOS)
			resolvedTd, err := os.Getwd()
			if err != nil {
				t.Fatal("Failed to get current working directory:", err)
			}
			vm, err := tc.vmm.NewInstance(ctx, resolvedTd)
			if err != nil {
				t.Fatal("Failed to create VM instance:", err)
			}

			if err := vm.Start(ctx, startOpts...); err != nil {
				t.Fatal("Failed to start VM instance:", err)
			}

			t.Cleanup(func() {
				vm.Shutdown(ctx)
				// Disable the log writer after shutdown so any background
				// goroutine still draining the console FIFO cannot call
				// t.Log() on a completed test.
				lw.disable()
			})

			runTest(t, vm)
		})
	}
}
