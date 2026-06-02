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

// DumpVMDKDescriptorToFile writes a VMDK descriptor to path atomically.
// It creates a uniquely-named temporary file in the same directory as path,
// syncs it to disk, and renames it into place. This ensures that a concurrent
// reader never observes a partially-written descriptor and that two concurrent
// writers do not clobber each other's temporary file.
func DumpVMDKDescriptorToFile(path string, cid uint32, devices []string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "merged_fs.vmdk.*.tmp")
	if err != nil {
		return err
	}
	tmpName := f.Name()

	werr := DumpVMDKDescriptor(f, cid, devices)
	serr := f.Sync()
	cerr := f.Close()

	if werr != nil {
		os.Remove(tmpName)
		return werr
	}
	if serr != nil {
		os.Remove(tmpName)
		return serr
	}
	if cerr != nil {
		os.Remove(tmpName)
		return cerr
	}
	return os.Rename(tmpName, path)
}
