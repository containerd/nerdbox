//go:build !windows

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

import "github.com/ebitengine/purego"

func dlOpen(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

func dlClose(handle uintptr) error {
	return purego.Dlclose(handle)
}

func registerLibFunc(fn interface{}, handle uintptr, name string) {
	purego.RegisterLibFunc(fn, handle, name)
}

// registerOptionalLibFunc resolves name in the library handle and binds fn
// to it when present. It returns false (without panicking) when the symbol
// is not exported, so callers can leave the function pointer nil and probe
// for it at call time.
func registerOptionalLibFunc(fn interface{}, handle uintptr, name string) bool {
	addr, err := purego.Dlsym(handle, name)
	if err != nil || addr == 0 {
		return false
	}
	purego.RegisterFunc(fn, addr)
	return true
}
