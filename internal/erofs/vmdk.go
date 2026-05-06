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
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	max2GbExtentSectors = 0x80000000 >> 9
	sectorsPerTrack     = 63
	numberHeads         = 16
	subformat           = "twoGbMaxExtentFlat"
	adapterType         = "ide"
	hwVersion           = "4"
)

// vmdkDescAddExtent writes extent lines to the writer.
// Each extent line follows the format: RW <count> FLAT "<filename>" <offset>
// A single device > 2 GiB is split across multiple RW lines, each referencing
// the same filename with increasing sector offsets.
func vmdkDescAddExtent(w io.Writer, sectors uint64, filename string, offset uint64) error {
	for sectors > 0 {
		count := min(sectors, max2GbExtentSectors)
		_, err := fmt.Fprintf(w, "RW %d FLAT \"%s\" %d\n", count, filename, offset)
		if err != nil {
			return err
		}
		offset += count
		sectors -= count
	}
	return nil
}

// vmdkDescAddZeroExtent writes "RW <count> ZERO" extent lines to the writer,
// splitting at the 2 GiB extent boundary. ZERO extents represent zero-filled
// regions of the virtual disk without referencing a backing file.
func vmdkDescAddZeroExtent(w io.Writer, sectors uint64) error {
	for sectors > 0 {
		count := min(sectors, max2GbExtentSectors)

		_, err := fmt.Fprintf(w, "RW %d ZERO\n", count)
		if err != nil {
			return err
		}
		sectors -= count
	}
	return nil
}

// DumpVMDKDescriptor writes a monolithicFlat VMDK descriptor to w.
// Extent path strings are written verbatim from devices.
func DumpVMDKDescriptor(w io.Writer, cid uint32, devices []string) error {
	parentCID := uint32(0xffffffff)

	_, err := fmt.Fprintf(w, `# Disk DescriptorFile
version=1
CID=%08x
parentCID=%08x
createType="%s"

# Extent description
`, cid, parentCID, subformat)
	if err != nil {
		return err
	}

	totalSectors := uint64(0)

	for _, d := range devices {
		fi, err := os.Stat(d)
		if err != nil {
			return err
		}
		sectors := uint64(fi.Size()) >> 9
		if err := vmdkDescAddExtent(w, sectors, d, 0); err != nil {
			return err
		}
		totalSectors += sectors
	}

	cylinders := (totalSectors + sectorsPerTrack*numberHeads - 1) / (sectorsPerTrack * numberHeads)
	_, err = fmt.Fprintf(w, `

# The Disk Data Base
#DDB

ddb.virtualHWVersion = "%s"
ddb.geometry.cylinders = "%d"
ddb.geometry.heads = "%d"
ddb.geometry.sectors = "63"
ddb.adapterType = "%s"
`, hwVersion, cylinders, numberHeads, adapterType)
	return err
}

func DumpVMDKDescriptorToFile(vmdkdesc string, cid uint32, devices []string) error {
	f, err := os.Create(vmdkdesc)
	if err != nil {
		return err
	}
	err = DumpVMDKDescriptor(f, cid, devices)
	f.Close()
	if err != nil {
		defer os.Remove(vmdkdesc)
		return err
	}
	return nil
}

// gptUseZeroExtents controls whether DumpGPTVMDKDescriptorToFile emits
// "RW <count> ZERO" extents (preferred) or generates a shared zero-filled
// padding file referenced via FLAT extents.
//
// libkrun's twoGbMaxExtentFlat parser is expected to support ZERO extents;
// flip this default if a particular libkrun build does not.
var gptUseZeroExtents = true

// DumpGPTVMDKDescriptor writes a VMDK descriptor whose virtual disk is a
// GPT-partitioned image with one partition per layout.Partitions entry.
//
// headerPath should reference a 34-sector blob produced by Layout.WriteHeader
// covering LBAs 0..33 (protective MBR, primary GPT header, primary entry array).
//
// When padFile is empty, padding regions are emitted as "RW <count> ZERO"
// extents. Otherwise padFile must be a zero-filled file of at least max
// padding-region size; padding regions are emitted as FLAT extents reading
// from offset 0 of padFile.
func DumpGPTVMDKDescriptor(w io.Writer, cid uint32, layout Layout, headerPath, padFile string) error {
	parentCID := uint32(0xffffffff)

	if _, err := fmt.Fprintf(w, `# Disk DescriptorFile
version=1
CID=%08x
parentCID=%08x
createType="%s"

# Extent description
`, cid, parentCID, subformat); err != nil {
		return err
	}

	emitPadding := func(sectors uint64) error {
		if sectors == 0 {
			return nil
		}
		if padFile == "" {
			return vmdkDescAddZeroExtent(w, sectors)
		}
		// FLAT extent reading zeros from offset 0 of the shared pad file.
		// vmdkDescAddExtent splits at max2GbExtentSectors automatically;
		// each split line still reads from offset 0 of the same file, which
		// is correct because the entire pad file is zero-filled.
		return vmdkDescAddExtent(w, sectors, padFile, 0)
	}

	// LBA 0..33: protective MBR + primary GPT header + primary entry array.
	if err := vmdkDescAddExtent(w, uint64(gptReservedSectors), headerPath, 0); err != nil {
		return err
	}

	prevEnd := uint64(gptReservedSectors)
	for _, p := range layout.Partitions {
		if p.FirstLBA > prevEnd {
			if err := emitPadding(p.FirstLBA - prevEnd); err != nil {
				return err
			}
		}
		if err := vmdkDescAddExtent(w, p.SectorCount(), p.Source, 0); err != nil {
			return err
		}
		prevEnd = p.LastLBA + 1
	}

	// Trailing padding to fill to TotalSectors (the disk ends at the 1 MiB
	// boundary after the last partition; no secondary GPT is written).
	if layout.TotalSectors > prevEnd {
		if err := emitPadding(layout.TotalSectors - prevEnd); err != nil {
			return err
		}
	}

	cylinders := (layout.TotalSectors + sectorsPerTrack*numberHeads - 1) / (sectorsPerTrack * numberHeads)
	_, err := fmt.Fprintf(w, `

# The Disk Data Base
#DDB

ddb.virtualHWVersion = "%s"
ddb.geometry.cylinders = "%d"
ddb.geometry.heads = "%d"
ddb.geometry.sectors = "63"
ddb.adapterType = "%s"
`, hwVersion, cylinders, numberHeads, adapterType)
	return err
}

// DumpGPTVMDKDescriptorToFile generates a GPT-partitioned VMDK at vmdkPath
// that references the supplied erofs layer files as separate partitions on
// a single virtual disk. A header blob and (when ZERO extents are not in use)
// a shared padding file are written next to vmdkPath; no secondary GPT is
// written since the disk is synthetic and read-only.
//
// The vmdk filename's extension is replaced with "_header.bin" and "_pad.bin"
// suffixes to derive the auxiliary filenames; e.g. for
// "/x/merged_fs_gpt.vmdk" the auxiliary files are "/x/merged_fs_gpt_header.bin"
// and (if applicable) "/x/merged_fs_gpt_pad.bin".
func DumpGPTVMDKDescriptorToFile(vmdkPath string, cid uint32, devices []string) error {
	layout, err := ComputeLayout(devices)
	if err != nil {
		return err
	}

	dir := filepath.Dir(vmdkPath)
	base := strings.TrimSuffix(filepath.Base(vmdkPath), filepath.Ext(vmdkPath))
	headerPath := filepath.Join(dir, base+"_header.bin")

	if err := writeBlob(headerPath, layout.WriteHeader); err != nil {
		return err
	}

	// cleanup removes all generated auxiliary files and the descriptor on
	// any error path after the header has been written.
	cleanup := func() {
		os.Remove(headerPath)
		os.Remove(vmdkPath)
	}

	var padFile string
	if !gptUseZeroExtents {
		// The maximum single padding region is bounded by gptAlignSectors
		// (1 MiB minus a sector); a 1 MiB pad file always suffices.
		padFile = filepath.Join(dir, base+"_pad.bin")
		if err := writePadFile(padFile, gptAlignSectors*gptSectorSize); err != nil {
			cleanup()
			return err
		}
		orig := cleanup
		cleanup = func() { orig(); os.Remove(padFile) }
	}

	f, err := os.Create(vmdkPath)
	if err != nil {
		cleanup()
		return err
	}
	if err := DumpGPTVMDKDescriptor(f, cid, layout, headerPath, padFile); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	return nil
}

func writeBlob(path string, fn func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writePadFile creates a sparse zero-filled file of the requested size.
func writePadFile(path string, size uint64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
