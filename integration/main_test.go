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
	"testing"

	"github.com/dmcgowan/nerdbox/internal/vm"
	"github.com/dmcgowan/nerdbox/internal/vm/runvm"
)

func TestMain(m *testing.M) {
	var err error

	absPath, err := filepath.Abs("../build")
	if err != nil {
		log.Fatalf("Failed to resolve build path: %v", err)
	}
	if err := os.Setenv("PATH", absPath+":"+os.Getenv("PATH")); err != nil {
		log.Fatalf("Failed to set PATH environment variable: %v", err)
	}

	r := m.Run()

	os.Exit(r)
}

func runWithVM(t *testing.T, runTest func(*testing.T, vm.Instance)) {
	for _, tc := range []struct {
		name   string
		create func() (vm.Instance, error)
	}{
		{
			name:   "run_vminitd",
			create: runvm.NewVMInstance,
		},
		/*
			{
				name: "libkrun",
				create: func() (vm.Instance, error) {
					return libkrun.NewVMInstance(libkrun.DebugLevel)
				},
			},
		*/
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm, err := tc.create()
			if err != nil {
				t.Fatal("Failed to create VM instance:", err)
			}

			f := filepath.Join(t.TempDir(), "vminitd.sock")
			if err := vm.Start(t.Context(), f, nil); err != nil {
				t.Fatal("Failed to start VM instance:", err)
			}

			t.Cleanup(func() {
				vm.Shutdown(t.Context())
			})

			runTest(t, vm)
		})
	}
}
