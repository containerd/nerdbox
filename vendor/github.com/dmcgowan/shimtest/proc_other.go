//go:build !linux && !windows

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

package shimtest

import (
	"errors"
	"runtime"
	"testing"
)

// readRSS is unimplemented on non-Linux platforms. The Linux version
// reads /proc/<pid>/status; macOS would use task_info(), Windows would
// use GetProcessMemoryInfo. Callers handle the error by skipping RSS
// monitoring (StressSuite.testExec calls t.Skipf).
func readRSS(pid int) (int64, error) {
	return 0, errors.New("readRSS is not implemented on " + runtime.GOOS)
}

// shimCmdlineInfo is unimplemented on non-Linux platforms. Returns an
// empty string so leak-detection error messages degrade gracefully.
func shimCmdlineInfo(pid int) string {
	return ""
}

// shimPIDs is unimplemented on non-Linux platforms. Returns nil so
// leak detection silently degrades to a no-op (shimPIDs(nil)+pidDiff
// reports an empty leak set). macOS would use sysctl KERN_PROC,
// Windows would use Toolhelp32Snapshot.
func shimPIDs(t *testing.T, binary string) map[int]struct{} {
	t.Helper()
	t.Logf("shim leak detection is unsupported on %s", runtime.GOOS)
	return nil
}
