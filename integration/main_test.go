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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/nerdbox/internal/vm"
	"github.com/containerd/nerdbox/internal/vm/libkrun"
)

func TestMain(m *testing.M) {
	e, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}
	exeDir := filepath.Dir(e)
	paths := filepath.SplitList(os.Getenv("PATH"))
	for _, p := range []string{
		"../_output",
		".",
	} {
		absPath := filepath.Clean(filepath.Join(exeDir, p))
		// Prepend to slice
		paths = append(paths, "")
		copy(paths[1:], paths)
		paths[0] = absPath
	}
	log.Printf("%v", paths)
	if err := os.Setenv("PATH", strings.Join(paths, string(filepath.ListSeparator))); err != nil {
		log.Fatalf("Failed to set PATH environment variable: %v", err)
	}

	r := m.Run()

	os.Exit(r)
}

func runWithVM(t *testing.T, runTest func(*testing.T, vm.Instance)) {
	for _, tc := range []struct {
		name string
		vmm  vm.Manager
	}{
		{
			name: "libkrun",
			vmm:  libkrun.NewManager(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			td := t.TempDir()
			t.Chdir(td)
			// Use Getwd to resolve symlinks (e.g., /var -> /private/var on macOS)
			resolvedTd, err := os.Getwd()
			if err != nil {
				t.Fatal("Failed to get current working directory:", err)
			}
			vm, err := tc.vmm.NewInstance(t.Context(), resolvedTd)
			if err != nil {
				t.Fatal("Failed to create VM instance:", err)
			}

			if err := vm.Start(t.Context()); err != nil {
				t.Fatal("Failed to start VM instance:", err)
			}

			t.Cleanup(func() {
				vm.Shutdown(t.Context())
			})

			runTest(t, vm)
		})
	}
}
