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

package libkrun

import (
	"fmt"
	"syscall"

	"github.com/ebitengine/purego"
)

func dlOpen(path string) (uintptr, error) {
	h, err := syscall.LoadLibrary(path)
	if err != nil {
		return 0, err
	}
	return uintptr(h), nil
}

func dlClose(_ uintptr) error {
	// On Windows, krun_start_enter may still be executing inside the library
	// when Shutdown is called — krun_free_ctx initiates the VM stop but does
	// not synchronously join the calling goroutine. Calling FreeLibrary while
	// a goroutine is still inside the DLL causes an access violation.
	//
	// Since nerdbox runs exactly one VM per process (the shim model), the
	// library handle is released naturally when the process exits. There is
	// no need to call FreeLibrary explicitly.
	return nil
}

func registerLibFunc(fn interface{}, handle uintptr, name string) {
	addr, err := syscall.GetProcAddress(syscall.Handle(handle), name)
	if err != nil {
		panic(fmt.Sprintf("failed to find %s: %v", name, err))
	}
	purego.RegisterFunc(fn, addr)
}

// registerOptionalLibFunc resolves name in the library handle and binds fn
// to it when present. It returns false (without panicking) when the symbol
// is not exported, so callers can leave the function pointer nil and probe
// for it at call time.
func registerOptionalLibFunc(fn interface{}, handle uintptr, name string) bool {
	addr, err := syscall.GetProcAddress(syscall.Handle(handle), name)
	if err != nil || addr == 0 {
		return false
	}
	purego.RegisterFunc(fn, uintptr(addr))
	return true
}
