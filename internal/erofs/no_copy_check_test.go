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

package erofs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestNoLayerDataCopy is a manual / informational test: it builds a GPT
// VMDK from 30 sparse 64-MiB layer files and reports the actual disk usage
// of every file produced. It demonstrates that the auxiliary files are
// tiny (a few KiB) and independent of layer count; layer data is referenced
// in place rather than copied.
//
// Skipped by default; set NERDBOX_MANUAL_TESTS=1 to run:
//
//	NERDBOX_MANUAL_TESTS=1 go test -run TestNoLayerDataCopy -v ./internal/erofs/
func TestNoLayerDataCopy(t *testing.T) {
	if os.Getenv("NERDBOX_MANUAL_TESTS") == "" {
		t.Skip("set NERDBOX_MANUAL_TESTS=1 to run")
	}
	const numLayers = 30
	const layerSize = 64 * 1024 * 1024 // 64 MiB sparse per layer

	tmp := t.TempDir()
	var devices []string
	for i := 0; i < numLayers; i++ {
		p := filepath.Join(tmp, fmt.Sprintf("layer-%03d.erofs", i))
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		// Sparse: Truncate sets the file size without allocating blocks.
		if err := f.Truncate(layerSize); err != nil {
			t.Fatal(err)
		}
		f.Close()
		devices = append(devices, p)
	}

	vmdkPath := filepath.Join(tmp, "merged_fs_gpt.vmdk")
	if err := DumpGPTVMDKDescriptorToFile(vmdkPath, 0xfffffffe, devices); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}

	var totalLogical, totalActual, auxLogical, auxActual int64
	t.Logf("%-32s %14s %14s", "file", "size (B)", "actual (B)")
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("Stat_t unavailable")
		}
		actual := stat.Blocks * 512
		isAux := strings.HasPrefix(e.Name(), "merged_fs_gpt") ||
			!strings.HasSuffix(e.Name(), ".erofs")
		marker := "      "
		if isAux {
			marker = " (aux)"
		}
		t.Logf("%-32s %14d %14d%s", e.Name(), fi.Size(), actual, marker)
		totalLogical += fi.Size()
		totalActual += actual
		if isAux {
			auxLogical += fi.Size()
			auxActual += actual
		}
	}

	t.Logf("--")
	t.Logf("layers:       %d", numLayers)
	t.Logf("logical sum:  %d B  (%d MiB)", totalLogical, totalLogical/1024/1024)
	t.Logf("actual sum:   %d B  (%d KiB)", totalActual, totalActual/1024)
	t.Logf("aux logical:  %d B", auxLogical)
	t.Logf("aux actual:   %d B  (%d KiB)", auxActual, auxActual/1024)

	// Sanity check: the auxiliary files combined must not store anything
	// close to a layer's worth of data. For 64 MiB layers a copy would
	// blow this up by ~64 MiB per layer; a no-copy implementation keeps
	// auxiliary disk usage in the tens-of-KiB range.
	const maxAuxKiB = 1024 // 1 MiB; in practice we expect ~33 KiB
	if auxActual/1024 > maxAuxKiB {
		t.Fatalf("auxiliary files use %d KiB of disk; expected < %d KiB (no-copy invariant violated)",
			auxActual/1024, maxAuxKiB)
	}
}
