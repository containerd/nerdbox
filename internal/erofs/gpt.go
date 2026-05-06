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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"unicode/utf16"
)

// Constants describing the GPT layout used for stacked erofs layer disks.
const (
	// gptSectorSize is the LBA size used by the synthetic VMDK-backed disk.
	// VMDK descriptors express extents and partition LBAs in 512-byte
	// sectors, so we always stick with this size regardless of the host or
	// the in-VM erofs filesystem block size.
	gptSectorSize = 512

	// gptAlignSectors aligns each partition to a 1 MiB boundary, which is
	// also a multiple of the 4 KiB erofs filesystem block size.
	gptAlignSectors = 2048

	// gptPartitionEntries is the standard partition entry count.
	gptPartitionEntries = 128

	// gptPartitionEntrySize is the size of each partition entry, in bytes.
	gptPartitionEntrySize = 128

	// gptPartitionArraySectors is the number of sectors covered by the
	// 128-entry partition array (128 * 128 = 16384 bytes = 32 sectors).
	gptPartitionArraySectors = (gptPartitionEntries * gptPartitionEntrySize) / gptSectorSize

	// gptHeaderSize is the size of the on-disk GPT header structure.
	gptHeaderSize = 92

	// gptHeaderRevision is the GPT 1.0 revision marker (major=1, minor=0).
	gptHeaderRevision uint32 = 0x00010000

	// gptReservedSectors is the number of sectors reserved at the head of
	// the disk before the first usable LBA: LBA 0 (protective MBR) + LBA 1
	// (primary header) + LBA 2..33 (primary partition entry array).
	gptReservedSectors = 1 + 1 + gptPartitionArraySectors
)

var (
	// gptSignature is the "EFI PART" magic at the start of the GPT header.
	gptSignature = [8]byte{'E', 'F', 'I', ' ', 'P', 'A', 'R', 'T'}

	// gptTypeLinuxFS is the type GUID for a generic Linux filesystem data
	// partition (canonical 0FC63DAF-8483-4772-8E79-3D69D8477DE4) encoded
	// in the mixed-endian on-disk form used by GPT.
	gptTypeLinuxFS = [16]byte{
		0xAF, 0x3D, 0xC6, 0x0F,
		0x83, 0x84,
		0x72, 0x47,
		0x8E, 0x79,
		0x3D, 0x69, 0xD8, 0x47, 0x7D, 0xE4,
	}

	// errGPTPartitionLimit is returned when more partitions are requested
	// than fit in a standard 128-entry GPT partition array.
	errGPTPartitionLimit = errors.New("erofs: too many layers for a GPT partition table")

	// errGPTUnaligned is returned when a layer file's size is not a
	// multiple of the GPT sector size.
	errGPTUnaligned = errors.New("erofs: layer file size is not sector-aligned")
)

// Partition describes a single GPT partition slot for an erofs layer.
type Partition struct {
	GUID      [16]byte
	TypeGUID  [16]byte
	FirstLBA  uint64
	LastLBA   uint64
	Name      string
	Source    string
	SizeBytes uint64
}

// SectorCount returns the number of 512-byte sectors covered by the partition.
func (p Partition) SectorCount() uint64 {
	return p.LastLBA - p.FirstLBA + 1
}

// Layout describes the GPT layout for a list of erofs layer files.
type Layout struct {
	SectorSize   uint64
	AlignSectors uint64
	TotalSectors uint64
	DiskGUID     [16]byte
	Partitions   []Partition
}

// FirstUsableLBA returns the first LBA available for partition data.
func (l Layout) FirstUsableLBA() uint64 {
	return uint64(gptReservedSectors)
}

// LastUsableLBA returns the last LBA available for partition data.
func (l Layout) LastUsableLBA() uint64 {
	return l.TotalSectors - 1
}

// ComputeLayout builds a GPT Layout for the supplied erofs layer files.
// The first path becomes partition 1, the second becomes partition 2, and
// so on. Layer file sizes are read via os.Stat.
func ComputeLayout(layerPaths []string) (Layout, error) {
	if len(layerPaths) > gptPartitionEntries {
		return Layout{}, fmt.Errorf("%w: %d > %d", errGPTPartitionLimit, len(layerPaths), gptPartitionEntries)
	}

	sizes := make([]uint64, len(layerPaths))
	for i, p := range layerPaths {
		fi, err := os.Stat(p)
		if err != nil {
			return Layout{}, err
		}
		size := uint64(fi.Size())
		if size == 0 || size%gptSectorSize != 0 {
			return Layout{}, fmt.Errorf("%w: %s size %d", errGPTUnaligned, p, size)
		}
		sizes[i] = size
	}

	parts := make([]Partition, len(layerPaths))
	cursor := uint64(gptAlignSectors) // partition 1 starts at LBA 2048 (1 MiB)
	for i, p := range layerPaths {
		sectors := sizes[i] / gptSectorSize
		first := cursor
		last := first + sectors - 1
		parts[i] = Partition{
			GUID:      derivePartGUID(p, sizes[i], i),
			TypeGUID:  gptTypeLinuxFS,
			FirstLBA:  first,
			LastLBA:   last,
			Name:      fmt.Sprintf("erofs-layer-%d", i),
			Source:    p,
			SizeBytes: sizes[i],
		}
		// Next partition slot starts at the next 1 MiB boundary.
		cursor = alignUp(last+1, gptAlignSectors)
	}

	// The disk ends at the 1 MiB boundary after the last partition.
	// We omit the secondary GPT: this disk is synthetic and read-only,
	// so recovery via the backup is not needed.
	total := cursor

	return Layout{
		SectorSize:   gptSectorSize,
		AlignSectors: gptAlignSectors,
		TotalSectors: total,
		DiskGUID:     deriveDiskGUID(layerPaths, sizes),
		Partitions:   parts,
	}, nil
}

// alignUp returns v rounded up to the nearest multiple of align.
// align must be non-zero.
func alignUp(v, align uint64) uint64 {
	return ((v + align - 1) / align) * align
}

// WriteHeader writes the protective MBR + primary GPT header + primary
// partition entry array, totalling gptReservedSectors (34) sectors.
// The secondary GPT is omitted: this disk is synthetic and read-only,
// so backup recovery is not needed.
func (l Layout) WriteHeader(w io.Writer) error {
	// LBA 0: protective MBR.
	mbr := make([]byte, gptSectorSize)
	// Single MBR partition entry at offset 446 covering the full disk and
	// marking the OS type as 0xEE (GPT protective).
	mbr[446+0] = 0x00 // boot indicator
	mbr[446+1] = 0x00 // starting CHS head
	mbr[446+2] = 0x02 // starting CHS sector (sector 2 = LBA 1)
	mbr[446+3] = 0x00 // starting CHS cylinder
	mbr[446+4] = 0xEE // OS type: GPT protective
	mbr[446+5] = 0xFF // ending CHS head
	mbr[446+6] = 0xFF // ending CHS sector
	mbr[446+7] = 0xFF // ending CHS cylinder
	binary.LittleEndian.PutUint32(mbr[446+8:], 1)
	sizeLBA := l.TotalSectors - 1
	if sizeLBA > 0xFFFFFFFF {
		sizeLBA = 0xFFFFFFFF
	}
	binary.LittleEndian.PutUint32(mbr[446+12:], uint32(sizeLBA))
	mbr[510] = 0x55
	mbr[511] = 0xAA
	if _, err := w.Write(mbr); err != nil {
		return err
	}

	// Build the partition array first; its CRC feeds into the header.
	arr := l.partitionArrayBytes()

	// LBA 1: primary GPT header.
	hdr := l.headerBytes(arr)
	if _, err := w.Write(hdr); err != nil {
		return err
	}

	// LBA 2..33: primary partition entry array.
	if _, err := w.Write(arr); err != nil {
		return err
	}
	return nil
}

// partitionArrayBytes returns the serialized 32-sector partition entry
// array containing all partitions in the layout, with unused slots zeroed.
func (l Layout) partitionArrayBytes() []byte {
	const arrSize = gptPartitionEntries * gptPartitionEntrySize
	arr := make([]byte, arrSize)
	for i, p := range l.Partitions {
		off := i * gptPartitionEntrySize
		copy(arr[off+0:off+16], p.TypeGUID[:])
		copy(arr[off+16:off+32], p.GUID[:])
		binary.LittleEndian.PutUint64(arr[off+32:], p.FirstLBA)
		binary.LittleEndian.PutUint64(arr[off+40:], p.LastLBA)
		// Attribute flags at bytes 48..55 left at zero.
		writePartName(arr[off+56:off+128], p.Name)
	}
	return arr
}

// headerBytes returns one sector containing the primary GPT header.
// altLBA is set to 0 because we omit the secondary GPT.
func (l Layout) headerBytes(arr []byte) []byte {
	hdr := make([]byte, gptSectorSize)
	copy(hdr[0:8], gptSignature[:])
	binary.LittleEndian.PutUint32(hdr[8:], gptHeaderRevision)
	binary.LittleEndian.PutUint32(hdr[12:], gptHeaderSize)
	// hdr[16:20] is the header CRC32, filled in below.
	// hdr[20:24] is reserved (must be zero).

	binary.LittleEndian.PutUint64(hdr[24:], 1) // MyLBA = 1
	binary.LittleEndian.PutUint64(hdr[32:], 0) // AlternateLBA = 0 (no secondary)
	binary.LittleEndian.PutUint64(hdr[40:], l.FirstUsableLBA())
	binary.LittleEndian.PutUint64(hdr[48:], l.LastUsableLBA())
	copy(hdr[56:72], l.DiskGUID[:])
	binary.LittleEndian.PutUint64(hdr[72:], 2) // PartitionEntryLBA = 2
	binary.LittleEndian.PutUint32(hdr[80:], uint32(gptPartitionEntries))
	binary.LittleEndian.PutUint32(hdr[84:], uint32(gptPartitionEntrySize))
	binary.LittleEndian.PutUint32(hdr[88:], crc32.ChecksumIEEE(arr))

	// Header CRC is computed over the first gptHeaderSize bytes with the
	// CRC field zeroed (it currently is).
	binary.LittleEndian.PutUint32(hdr[16:], crc32.ChecksumIEEE(hdr[:gptHeaderSize]))
	return hdr
}

// writePartName UTF-16LE encodes name into buf, NUL-padding the remainder.
// Truncation is silent if the encoded form does not fit.
func writePartName(buf []byte, name string) {
	enc := utf16.Encode([]rune(name))
	maxUnits := len(buf) / 2
	if len(enc) > maxUnits {
		enc = enc[:maxUnits]
	}
	for i, r := range enc {
		binary.LittleEndian.PutUint16(buf[i*2:i*2+2], r)
	}
}

// deriveDiskGUID returns a deterministic disk GUID derived from the layer
// paths and sizes. The same set of layers produces the same GUID, keeping
// the synthetic VMDK reproducible across shim invocations.
func deriveDiskGUID(paths []string, sizes []uint64) [16]byte {
	h := sha256.New()
	io.WriteString(h, "nerdbox/erofs/disk")
	for i, p := range paths {
		fmt.Fprintf(h, "\n%s:%d", p, sizes[i])
	}
	var guid [16]byte
	copy(guid[:], h.Sum(nil))
	setRFC4122Bits(&guid)
	return guid
}

// derivePartGUID returns a deterministic partition GUID for a layer.
func derivePartGUID(path string, size uint64, index int) [16]byte {
	h := sha256.New()
	io.WriteString(h, "nerdbox/erofs/part")
	fmt.Fprintf(h, "\n%d:%s:%d", index, path, size)
	var guid [16]byte
	copy(guid[:], h.Sum(nil))
	setRFC4122Bits(&guid)
	return guid
}

// setRFC4122Bits sets the version (4) and RFC 4122 variant bits in the
// supplied 16 bytes. Because GPT stores GUIDs in mixed-endian form,
// canonical UUID byte 6 (the version nibble) maps to on-disk byte 7, and
// canonical byte 8 (the variant) maps to on-disk byte 8.
//
// The kernel's GPT parser does not enforce these bits, so this is purely
// for niceness when the GUID is read back as a canonical UUID string.
func setRFC4122Bits(g *[16]byte) {
	g[7] = (g[7] & 0x0F) | 0x40 // version 4
	g[8] = (g[8] & 0x3F) | 0x80 // RFC 4122 variant
}
