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
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// readRSS returns the Resident Set Size of pid in bytes.
// Uses ps(1) which is available on macOS and most non-Linux Unixes.
func readRSS(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps rss for pid %d: %w", pid, err)
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse rss for pid %d: %w", pid, err)
	}
	return kb * 1024, nil
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
