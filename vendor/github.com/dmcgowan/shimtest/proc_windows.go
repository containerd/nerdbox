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

package shimtest

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS from psapi.h.
// GetProcessMemoryInfo is not yet wrapped by golang.org/x/sys/windows,
// so we call psapi.dll directly.
type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	psapi                  = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// readRSS returns the Working Set size (analogous to RSS) of the given
// pid in bytes using GetProcessMemoryInfo from psapi.dll.
//
// We try PROCESS_QUERY_LIMITED_INFORMATION first (available since Vista,
// requires fewer privileges than PROCESS_QUERY_INFORMATION|PROCESS_VM_READ)
// and fall back to the full access set if the limited form is denied.
// The shim is a grandchild process and may run with a different integrity
// level on Windows.
func readRSS(pid int) (int64, error) {
	// PROCESS_QUERY_LIMITED_INFORMATION (0x1000) suffices for
	// GetProcessMemoryInfo on Windows Vista and later.
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Fall back to the broader access set.
		h, err = windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
		if err != nil {
			return 0, fmt.Errorf("OpenProcess(%d): %w", pid, err)
		}
	}
	defer windows.CloseHandle(h)

	var mc processMemoryCounters
	mc.Cb = uint32(unsafe.Sizeof(mc))
	r, _, err := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.Cb))
	if r == 0 {
		return 0, fmt.Errorf("GetProcessMemoryInfo(%d): %w", pid, err)
	}
	return int64(mc.WorkingSetSize), nil
}

// shimCmdlineInfo is unimplemented on Windows. Reading another
// process's command line requires NtQueryInformationProcess + PEB
// traversal via ReadProcessMemory, which is intricate enough that the
// minimal payoff (extra context in leak-detection error messages) does
// not justify the complexity here. Returning "" makes leak detection
// degrade gracefully: leaked PIDs are still reported, just without the
// -namespace/-id annotation that proc_linux.go produces.
func shimCmdlineInfo(pid int) string {
	return ""
}

// shimPIDs returns the set of currently-live shim processes whose
// executable path matches binary, using CreateToolhelp32Snapshot.
func shimPIDs(t *testing.T, binary string) map[int]struct{} {
	t.Helper()

	// Resolve the target binary to an absolute path for comparison.
	target, err := filepath.Abs(binary)
	if err != nil {
		t.Logf("shimPIDs: cannot resolve binary path %q: %v (leak detection disabled)", binary, err)
		return nil
	}
	target = strings.ToLower(target)

	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Logf("shimPIDs: CreateToolhelp32Snapshot: %v (leak detection disabled)", err)
		return nil
	}
	defer windows.CloseHandle(snap)

	pids := make(map[int]struct{})
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	if err := windows.Process32First(snap, &entry); err != nil {
		return pids
	}
	for {
		// entry.ExeFile is just the base name; we need the full path.
		// Open the process and query its image file name.
		pid := entry.ProcessID
		if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid); err == nil {
			var buf [windows.MAX_PATH]uint16
			size := uint32(len(buf))
			if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil {
				exe := strings.ToLower(windows.UTF16ToString(buf[:size]))
				if exe == target {
					pids[int(pid)] = struct{}{}
				}
			}
			windows.CloseHandle(h)
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}
	return pids
}
