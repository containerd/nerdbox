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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extentLine is a parsed `RW <count> [FLAT "<file>" <offset> | ZERO]` line.
type extentLine struct {
	count    uint64
	kind     string // "FLAT" or "ZERO"
	filename string
	offset   uint64
}

// parseDescriptor extracts extent lines and computes the cumulative sector total.
func parseDescriptor(t *testing.T, raw string) []extentLine {
	t.Helper()
	var extents []extentLine
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "RW ") {
			continue
		}

		// Two formats: `RW N FLAT "file" off` or `RW N ZERO`.
		//
		// We must not use fmt.Sscanf with %q to extract the filename because
		// %q expects a Go-quoted string and will misinterpret Windows paths
		// containing backslashes (e.g. "C:\foo" → \f is not a valid escape).
		// Instead we parse by splitting on the double-quote delimiter, which
		// matches the raw quoting written by vmdkDescAddExtent.
		if strings.Contains(line, " FLAT ") {
			// Split on `"` to extract the filename between the quotes.
			parts := strings.SplitN(line, `"`, 3)
			if len(parts) != 3 {
				t.Fatalf("could not parse FLAT extent line: %q", line)
			}
			// parts[0] = `RW N FLAT `, parts[1] = filename, parts[2] = ` off`
			var count uint64
			var offset uint64
			if _, err := fmt.Sscanf(parts[0], "RW %d FLAT ", &count); err != nil {
				t.Fatalf("could not parse FLAT extent count in %q: %v", line, err)
			}
			fname := parts[1]
			// parts[2] is ` <offset>` or empty (offset defaults to 0).
			if rest := strings.TrimSpace(parts[2]); rest != "" {
				if _, err := fmt.Sscanf(rest, "%d", &offset); err != nil {
					t.Fatalf("could not parse FLAT extent offset in %q: %v", line, err)
				}
			}
			extents = append(extents, extentLine{count: count, kind: "FLAT", filename: fname, offset: offset})
			continue
		}

		var count uint64
		if zeroN, _ := fmt.Sscanf(line, "RW %d ZERO", &count); zeroN == 1 {
			extents = append(extents, extentLine{count: count, kind: "ZERO"})
			continue
		}
		t.Fatalf("could not parse extent line: %q", line)
	}
	return extents
}

func TestDumpGPTVMDKDescriptorZeroExtents(t *testing.T) {
	tmp := t.TempDir()

	a := makeLayerFile(t, tmp, "a.img", 4*1024*1024) // 4 MiB
	b := makeLayerFile(t, tmp, "b.img", 4*1024*1024)
	c := makeLayerFile(t, tmp, "c.img", 4*1024*1024)

	devices := []string{a, b, c}
	layout, err := ComputeLayout(devices)
	require.NoError(t, err)

	headerPath := filepath.Join(tmp, "h.bin")
	require.NoError(t, writeBlob(headerPath, layout.WriteHeader))

	var buf bytes.Buffer
	require.NoError(t, DumpGPTVMDKDescriptor(&buf, 0xfffffffe, layout, headerPath, ""))

	desc := buf.String()
	assert.Contains(t, desc, `createType="twoGbMaxExtentFlat"`)
	assert.Contains(t, desc, "ddb.adapterType")

	extents := parseDescriptor(t, desc)
	require.NotEmpty(t, extents)

	// First extent: gptReservedSectors of header file.
	assert.Equal(t, "FLAT", extents[0].kind)
	assert.Equal(t, headerPath, extents[0].filename)
	assert.Equal(t, uint64(gptReservedSectors), extents[0].count)
	assert.Equal(t, uint64(0), extents[0].offset)

	// Sum of sector counts equals layout.TotalSectors.
	var total uint64
	for _, e := range extents {
		total += e.count
	}
	assert.Equal(t, layout.TotalSectors, total,
		"sum of extent counts must equal total sectors")

	// At least one ZERO extent exists for padding (alignment skipped sectors).
	var sawZero bool
	for _, e := range extents {
		if e.kind == "ZERO" {
			sawZero = true
			break
		}
	}
	assert.True(t, sawZero, "expected at least one ZERO extent for alignment padding")

	// Each layer must appear as a FLAT extent of the right size at offset 0.
	for i, dev := range devices {
		var found bool
		want := layout.Partitions[i].SectorCount()
		for _, e := range extents {
			if e.kind == "FLAT" && e.filename == dev && e.count == want && e.offset == 0 {
				found = true
				break
			}
		}
		assert.True(t, found, "expected FLAT extent for device %s of %d sectors", dev, want)
	}
}

func TestDumpGPTVMDKDescriptorPaddingFile(t *testing.T) {
	tmp := t.TempDir()

	a := makeLayerFile(t, tmp, "a.img", 4*1024*1024)
	b := makeLayerFile(t, tmp, "b.img", 4*1024*1024)

	layout, err := ComputeLayout([]string{a, b})
	require.NoError(t, err)

	headerPath := filepath.Join(tmp, "h.bin")
	padPath := filepath.Join(tmp, "p.bin")
	require.NoError(t, writeBlob(headerPath, layout.WriteHeader))
	require.NoError(t, writePadFile(padPath, gptAlignSectors*gptSectorSize))

	var buf bytes.Buffer
	require.NoError(t, DumpGPTVMDKDescriptor(&buf, 0xfffffffe, layout, headerPath, padPath))

	extents := parseDescriptor(t, buf.String())
	require.NotEmpty(t, extents)

	// No ZERO extents when a padFile is provided.
	for _, e := range extents {
		assert.NotEqual(t, "ZERO", e.kind, "no ZERO extents allowed when padFile is set")
	}

	// At least one extent references the pad file.
	var sawPad bool
	for _, e := range extents {
		if e.kind == "FLAT" && e.filename == padPath {
			sawPad = true
			assert.Equal(t, uint64(0), e.offset, "pad extent must read from offset 0")
		}
	}
	assert.True(t, sawPad, "expected at least one FLAT extent referencing the pad file")

	// Sum of sectors still equals the total disk size.
	var total uint64
	for _, e := range extents {
		total += e.count
	}
	assert.Equal(t, layout.TotalSectors, total)
}

func TestDumpGPTVMDKDescriptorToFile(t *testing.T) {
	tmp := t.TempDir()

	devices := []string{
		makeLayerFile(t, tmp, "a.img", 4*1024*1024),
		makeLayerFile(t, tmp, "b.img", 4*1024*1024),
	}

	vmdkPath := filepath.Join(tmp, "merged_fs_gpt.vmdk")
	require.NoError(t, DumpGPTVMDKDescriptorToFile(vmdkPath, 0xfffffffe, devices))

	// Descriptor exists.
	_, err := os.Stat(vmdkPath)
	require.NoError(t, err)

	// Header blob must exist.
	hf, err := os.Stat(filepath.Join(tmp, "merged_fs_gpt_header.bin"))
	require.NoError(t, err)
	assert.Equal(t, int64(gptReservedSectors*gptSectorSize), hf.Size())

	// No tail blob is written (secondary GPT is omitted).
	_, err = os.Stat(filepath.Join(tmp, "merged_fs_gpt_tail.bin"))
	assert.True(t, os.IsNotExist(err), "tail blob must not be created")

	// With ZERO extents enabled (the default), no pad file is written.
	_, err = os.Stat(filepath.Join(tmp, "merged_fs_gpt_pad.bin"))
	assert.True(t, os.IsNotExist(err), "pad file must not be created when ZERO extents are used")
}

func TestDumpGPTVMDKDescriptorToFilePaddingFallback(t *testing.T) {
	t.Cleanup(func() {
		gptUseZeroExtents = true
	})
	gptUseZeroExtents = false

	tmp := t.TempDir()
	devices := []string{
		makeLayerFile(t, tmp, "a.img", 4*1024*1024),
		makeLayerFile(t, tmp, "b.img", 4*1024*1024),
	}

	vmdkPath := filepath.Join(tmp, "gpt.vmdk")
	require.NoError(t, DumpGPTVMDKDescriptorToFile(vmdkPath, 0xfffffffe, devices))

	pf, err := os.Stat(filepath.Join(tmp, "gpt_pad.bin"))
	require.NoError(t, err)
	assert.Equal(t, int64(gptAlignSectors*gptSectorSize), pf.Size(),
		"pad file must be 1 MiB to cover the largest possible alignment region")
}

func TestDumpGPTVMDKDescriptorExtentOrdering(t *testing.T) {
	// Verify that the descriptor lays out extents strictly in LBA order:
	// [header, padding?, layer1, padding?, layer2, ..., trailing padding].
	tmp := t.TempDir()

	devices := []string{
		makeLayerFile(t, tmp, "a.img", 4096),  // 1 sector aligned to 1 MiB → big trailing pad
		makeLayerFile(t, tmp, "b.img", 8192),  // 2 sectors aligned to 1 MiB → big trailing pad
		makeLayerFile(t, tmp, "c.img", 12288), // 3 sectors aligned to 1 MiB → big trailing pad
	}

	layout, err := ComputeLayout(devices)
	require.NoError(t, err)

	hdrPath := filepath.Join(tmp, "h.bin")
	require.NoError(t, writeBlob(hdrPath, layout.WriteHeader))

	var buf bytes.Buffer
	require.NoError(t, DumpGPTVMDKDescriptor(&buf, 0xfffffffe, layout, hdrPath, ""))

	extents := parseDescriptor(t, buf.String())

	// Walk extents accumulating LBAs; assert every layer extent appears at
	// the LBA the layout assigns to its partition.
	var lba uint64
	deviceLBA := map[string]uint64{}
	for _, e := range extents {
		if e.kind == "FLAT" {
			deviceLBA[e.filename] = lba
		}
		lba += e.count
	}
	assert.Equal(t, layout.TotalSectors, lba)

	for i, dev := range devices {
		assert.Equal(t, layout.Partitions[i].FirstLBA, deviceLBA[dev],
			"layer %d %s must start at partition LBA %d", i, dev, layout.Partitions[i].FirstLBA)
	}

	// Header extent must start at LBA 0.
	assert.Equal(t, uint64(0), deviceLBA[hdrPath])
}
