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
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeLayerFile creates a sparse zero-filled file of the requested size.
func makeLayerFile(t *testing.T, dir, name string, size int64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	return p
}

func TestComputeLayoutBasic(t *testing.T) {
	tmp := t.TempDir()
	a := makeLayerFile(t, tmp, "a.img", 64*1024*1024) // 64 MiB, aligned
	b := makeLayerFile(t, tmp, "b.img", 4*1024*1024)  // 4 MiB
	c := makeLayerFile(t, tmp, "c.img", 8*1024)       // 8 KiB

	l, err := ComputeLayout([]string{a, b, c})
	require.NoError(t, err)

	assert.Equal(t, uint64(gptSectorSize), l.SectorSize)
	assert.Equal(t, uint64(gptAlignSectors), l.AlignSectors)
	assert.Len(t, l.Partitions, 3)

	// First partition starts at LBA 2048 (1 MiB).
	assert.Equal(t, uint64(2048), l.Partitions[0].FirstLBA)
	// 64 MiB = 131072 sectors.
	assert.Equal(t, uint64(131072), l.Partitions[0].SectorCount())
	assert.Equal(t, uint64(2048+131072-1), l.Partitions[0].LastLBA)

	// Partition 2 starts at the next 1 MiB boundary (133120 = 65 * 2048).
	assert.Equal(t, uint64(2048+131072), l.Partitions[1].FirstLBA)
	// 4 MiB = 8192 sectors.
	assert.Equal(t, uint64(8192), l.Partitions[1].SectorCount())

	// Partition 3 starts at the next 1 MiB boundary after partition 2 ends.
	assert.Equal(t, l.Partitions[1].LastLBA+1, l.Partitions[2].FirstLBA)
	// 8 KiB = 16 sectors.
	assert.Equal(t, uint64(16), l.Partitions[2].SectorCount())

	// Total disk ends at the 1 MiB boundary after the last partition (no tail).
	expected := alignUp(l.Partitions[2].LastLBA+1, uint64(gptAlignSectors))
	assert.Equal(t, expected, l.TotalSectors)

	// Each partition gets the Linux fs data type GUID and a unique partition GUID.
	for i, p := range l.Partitions {
		assert.Equal(t, gptTypeLinuxFS, p.TypeGUID, "partition %d", i)
		assert.NotEqual(t, [16]byte{}, p.GUID, "partition %d", i)
		assert.Equal(t, fmt.Sprintf("erofs-layer-%d", i), p.Name)
	}

	// Disk GUID is non-zero and partition GUIDs differ from each other.
	assert.NotEqual(t, [16]byte{}, l.DiskGUID)
	assert.NotEqual(t, l.Partitions[0].GUID, l.Partitions[1].GUID)
	assert.NotEqual(t, l.Partitions[1].GUID, l.Partitions[2].GUID)
}

func TestComputeLayoutDeterministic(t *testing.T) {
	tmp := t.TempDir()
	a := makeLayerFile(t, tmp, "a.img", 4096)
	b := makeLayerFile(t, tmp, "b.img", 8192)

	l1, err := ComputeLayout([]string{a, b})
	require.NoError(t, err)
	l2, err := ComputeLayout([]string{a, b})
	require.NoError(t, err)

	assert.Equal(t, l1.DiskGUID, l2.DiskGUID)
	assert.Equal(t, l1.Partitions[0].GUID, l2.Partitions[0].GUID)
	assert.Equal(t, l1.Partitions[1].GUID, l2.Partitions[1].GUID)
}

func TestComputeLayoutUnaligned(t *testing.T) {
	tmp := t.TempDir()
	bad := makeLayerFile(t, tmp, "bad.img", 1234) // not sector-aligned

	_, err := ComputeLayout([]string{bad})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errGPTUnaligned), "want errGPTUnaligned, got %v", err)
}

func TestComputeLayoutEmptyFile(t *testing.T) {
	tmp := t.TempDir()
	zero := makeLayerFile(t, tmp, "zero.img", 0)

	_, err := ComputeLayout([]string{zero})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errGPTUnaligned), "want errGPTUnaligned, got %v", err)
}

func TestComputeLayoutTooMany(t *testing.T) {
	tmp := t.TempDir()
	paths := make([]string, gptPartitionEntries+1)
	for i := range paths {
		paths[i] = makeLayerFile(t, tmp, fmt.Sprintf("l%d.img", i), 4096)
	}

	_, err := ComputeLayout(paths)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errGPTPartitionLimit), "want errGPTPartitionLimit, got %v", err)
}

func TestWriteHeaderStructure(t *testing.T) {
	tmp := t.TempDir()
	a := makeLayerFile(t, tmp, "a.img", 4*1024*1024)
	b := makeLayerFile(t, tmp, "b.img", 4*1024*1024)

	l, err := ComputeLayout([]string{a, b})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, l.WriteHeader(&buf))
	require.Equal(t, gptReservedSectors*gptSectorSize, buf.Len(),
		"header blob must cover %d sectors", gptReservedSectors)

	data := buf.Bytes()

	// LBA 0: protective MBR.
	mbr := data[:gptSectorSize]
	assert.Equal(t, byte(0xEE), mbr[446+4], "MBR partition type must be 0xEE")
	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(mbr[446+8:]),
		"MBR starting LBA must be 1")
	expectedSize := uint32(l.TotalSectors - 1)
	if l.TotalSectors-1 > 0xFFFFFFFF {
		expectedSize = 0xFFFFFFFF
	}
	assert.Equal(t, expectedSize, binary.LittleEndian.Uint32(mbr[446+12:]),
		"MBR size in LBA must be totalSectors - 1 (capped at 0xFFFFFFFF)")
	assert.Equal(t, byte(0x55), mbr[510])
	assert.Equal(t, byte(0xAA), mbr[511])

	// LBA 1: primary GPT header.
	hdr := data[gptSectorSize : 2*gptSectorSize]
	assert.Equal(t, gptSignature[:], hdr[0:8])
	assert.Equal(t, gptHeaderRevision, binary.LittleEndian.Uint32(hdr[8:]))
	assert.Equal(t, uint32(gptHeaderSize), binary.LittleEndian.Uint32(hdr[12:]))
	assert.Equal(t, uint64(1), binary.LittleEndian.Uint64(hdr[24:]), "MyLBA")
	assert.Equal(t, uint64(0), binary.LittleEndian.Uint64(hdr[32:]), "AlternateLBA (no secondary GPT)")
	assert.Equal(t, uint64(gptReservedSectors), binary.LittleEndian.Uint64(hdr[40:]), "FirstUsableLBA")
	assert.Equal(t, l.TotalSectors-1, binary.LittleEndian.Uint64(hdr[48:]), "LastUsableLBA")
	assert.Equal(t, l.DiskGUID[:], hdr[56:72])
	assert.Equal(t, uint64(2), binary.LittleEndian.Uint64(hdr[72:]), "PartitionEntryLBA")
	assert.Equal(t, uint32(gptPartitionEntries), binary.LittleEndian.Uint32(hdr[80:]))
	assert.Equal(t, uint32(gptPartitionEntrySize), binary.LittleEndian.Uint32(hdr[84:]))

	// Header CRC validates: zero out the CRC field and recompute.
	hdrCheck := append([]byte(nil), hdr[:gptHeaderSize]...)
	storedHeaderCRC := binary.LittleEndian.Uint32(hdrCheck[16:])
	binary.LittleEndian.PutUint32(hdrCheck[16:], 0)
	assert.Equal(t, crc32.ChecksumIEEE(hdrCheck), storedHeaderCRC, "header CRC must validate")

	// LBA 2..33: partition entry array.
	arr := data[2*gptSectorSize:]
	require.Equal(t, gptPartitionArraySectors*gptSectorSize, len(arr))
	storedArrCRC := binary.LittleEndian.Uint32(hdr[88:])
	assert.Equal(t, crc32.ChecksumIEEE(arr), storedArrCRC, "partition array CRC must validate")

	// Partition entries: type GUID, partition GUID, first/last LBA.
	for i, p := range l.Partitions {
		off := i * gptPartitionEntrySize
		assert.Equal(t, p.TypeGUID[:], arr[off:off+16], "part %d type GUID", i)
		assert.Equal(t, p.GUID[:], arr[off+16:off+32], "part %d GUID", i)
		assert.Equal(t, p.FirstLBA, binary.LittleEndian.Uint64(arr[off+32:]),
			"part %d first LBA", i)
		assert.Equal(t, p.LastLBA, binary.LittleEndian.Uint64(arr[off+40:]),
			"part %d last LBA", i)
	}

	// Unused entries (after the last partition) must be all zero.
	for i := len(l.Partitions); i < gptPartitionEntries; i++ {
		off := i * gptPartitionEntrySize
		entry := arr[off : off+gptPartitionEntrySize]
		var zero [gptPartitionEntrySize]byte
		assert.Equal(t, zero[:], entry, "unused partition entry %d must be zeroed", i)
	}
}

func TestLinuxFSGUIDEncoding(t *testing.T) {
	// Canonical 0FC63DAF-8483-4772-8E79-3D69D8477DE4 in mixed-endian on-disk form.
	want := [16]byte{
		0xAF, 0x3D, 0xC6, 0x0F,
		0x83, 0x84,
		0x72, 0x47,
		0x8E, 0x79,
		0x3D, 0x69, 0xD8, 0x47, 0x7D, 0xE4,
	}
	assert.Equal(t, want, gptTypeLinuxFS)
}
