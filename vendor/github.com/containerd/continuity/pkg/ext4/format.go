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

// Package ext4 creates ext4 filesystem images.
package ext4

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// CreateOpt configures filesystem creation.
type CreateOpt func(*createConfig)

type createConfig struct {
	dirs []dirEntry
}

// dirEntry represents a directory to be created in the filesystem
// with specific permissions and ownership.
type dirEntry struct {
	Path string
	Mode fs.FileMode
	UID  uint32
	GID  uint32
}

// WithDir adds a directory to be created in the filesystem with the
// specified permissions and ownership. Parent directories are created
// automatically, inheriting the child's permissions.
func WithDir(path string, mode fs.FileMode, uid, gid uint32) CreateOpt {
	return func(c *createConfig) {
		c.dirs = append(c.dirs, dirEntry{Path: path, Mode: mode, UID: uid, GID: gid})
	}
}

// splitPath splits a path into its components, ignoring leading/trailing slashes.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

const (
	superblockOffset = 1024
	superblockSize   = 1024
	ext4Magic        = 0xEF53

	blockSize       = 4096
	inodeSize       = 256
	groupDescSize   = 64
	logBlockSize    = 2 // log2(4096/1024)
	blocksPerGroup  = 32768
	inodesPerBlock  = blockSize / inodeSize // 16
	extentMagic     = 0xF30A
	maxExtentLeaves = 4

	// Feature flags
	compatExtAttr     = 0x0008
	compatDirIndex    = 0x0020
	compatSparseSuper = 0x0200

	incompatFiletype = 0x0002
	incompatExtents  = 0x0040
	incompat64bit    = 0x0080
	incompatFlexBG   = 0x0200
	incompatCsumSeed = 0x2000

	roCompatSparseSuper  = 0x0001
	roCompatLargeFile    = 0x0002
	roCompatHugeFile     = 0x0008
	roCompatDirNlink     = 0x0020
	roCompatExtraIsize   = 0x0040
	roCompatMetadataCsum = 0x0400

	// Inode flags
	extentsFlag = 0x00080000

	// Block group flags
	bgInodeUninit = 0x0001
	bgBlockUninit = 0x0002
	bgInodeZeroed = 0x0004

	// Directory entry file types
	ftDir = 2

	// Default mount options
	defmXattrUser = 0x0004
	defmACL       = 0x0008

	// Hash version
	hashHalfMD4 = 1

	// Misc flags
	flagsSignedHash = 0x0001
)

// superblock represents the ext4 superblock structure.
// Field offsets match the Linux kernel's ext4_super_block struct.
type superblock struct {
	InodeCount        uint32     // 0x00
	BlockCountLo      uint32     // 0x04
	RBlockCountLo     uint32     // 0x08
	FreeBlockCountLo  uint32     // 0x0C
	FreeInodeCount    uint32     // 0x10
	FirstDataBlock    uint32     // 0x14
	LogBlockSize      uint32     // 0x18
	LogClusterSize    uint32     // 0x1C
	BlocksPerGroup    uint32     // 0x20
	ClustersPerGroup  uint32     // 0x24
	InodesPerGroup    uint32     // 0x28
	Mtime             uint32     // 0x2C
	Wtime             uint32     // 0x30
	MntCount          uint16     // 0x34
	MaxMntCount       uint16     // 0x36
	Magic             uint16     // 0x38
	State             uint16     // 0x3A
	Errors            uint16     // 0x3C
	MinorRevLevel     uint16     // 0x3E
	Lastcheck         uint32     // 0x40
	Checkinterval     uint32     // 0x44
	CreatorOS         uint32     // 0x48
	RevLevel          uint32     // 0x4C
	DefResUID         uint16     // 0x50
	DefResGID         uint16     // 0x52
	FirstIno          uint32     // 0x54
	InodeSize         uint16     // 0x58
	BlockGroupNr      uint16     // 0x5A
	FeatureCompat     uint32     // 0x5C
	FeatureIncompat   uint32     // 0x60
	FeatureRoCompat   uint32     // 0x64
	UUID              [16]byte   // 0x68
	VolumeName        [16]byte   // 0x78
	LastMounted       [64]byte   // 0x88
	AlgorithmBitmap   uint32     // 0xC8
	PreallocBlocks    uint8      // 0xCC
	PreallocDirBlocks uint8      // 0xCD
	ReservedGDTBlocks uint16     // 0xCE
	JournalUUID       [16]byte   // 0xD0
	JournalInum       uint32     // 0xE0
	JournalDev        uint32     // 0xE4
	LastOrphan        uint32     // 0xE8
	HashSeed          [4]uint32  // 0xEC
	DefHashVersion    uint8      // 0xFC
	JnlBackupType     uint8      // 0xFD
	DescSize          uint16     // 0xFE
	DefaultMountOpts  uint32     // 0x100
	FirstMetaBg       uint32     // 0x104
	MkfsTime          uint32     // 0x108
	JnlBlocks         [17]uint32 // 0x10C
	BlockCountHi      uint32     // 0x150
	RBlockCountHi     uint32     // 0x154
	FreeBlockCountHi  uint32     // 0x158
	MinExtraIsize     uint16     // 0x15C
	WantExtraIsize    uint16     // 0x15E
	Flags             uint32     // 0x160
	RaidStride        uint16     // 0x164
	MMPInterval       uint16     // 0x166
	MMPBlock          uint64     // 0x168
	RaidStripeWidth   uint32     // 0x170
	LogGroupsPerFlex  uint8      // 0x174
	ChecksumType      uint8      // 0x175
	EncryptionLevel   uint8      // 0x176
	ReservedPad       uint8      // 0x177
	KbytesWritten     uint64     // 0x178
	SnapshotInum      uint32     // 0x180
	SnapshotID        uint32     // 0x184
	SnapshotRBlocks   uint64     // 0x188
	SnapshotList      uint32     // 0x190
	ErrorCount        uint32     // 0x194
	FirstErrorTime    uint32     // 0x198
	FirstErrorIno     uint32     // 0x19C
	FirstErrorBlock   uint64     // 0x1A0
	FirstErrorFunc    [32]byte   // 0x1A8
	FirstErrorLine    uint32     // 0x1C8
	LastErrorTime     uint32     // 0x1CC
	LastErrorIno      uint32     // 0x1D0
	LastErrorLine     uint32     // 0x1D4
	LastErrorBlock    uint64     // 0x1D8
	LastErrorFunc     [32]byte   // 0x1E0
	MountOpts         [64]byte   // 0x200
	UsrQuotaInum      uint32     // 0x240
	GrpQuotaInum      uint32     // 0x244
	OverheadClusters  uint32     // 0x248
	BackupBGs         [2]uint32  // 0x24C
	EncryptAlgos      [4]uint8   // 0x254
	EncryptPWSalt     [16]byte   // 0x258
	LpfIno            uint32     // 0x268
	PrjQuotaInum      uint32     // 0x26C
	ChecksumSeed      uint32     // 0x270
	WtimeHi           uint8      // 0x274
	MtimeHi           uint8      // 0x275
	MkfsTimeHi        uint8      // 0x276
	LastcheckHi       uint8      // 0x277
	FirstErrorTimeHi  uint8      // 0x278
	LastErrorTimeHi   uint8      // 0x279
	FirstErrorErrcode uint8      // 0x27A
	LastErrorErrcode  uint8      // 0x27B
	Encoding          uint16     // 0x27C
	EncodingFlags     uint16     // 0x27E
	OrphanFileInum    uint32     // 0x280
	Reserved          [94]uint32 // 0x284
	Checksum          uint32     // 0x3FC
}

// groupDesc represents a 64-byte block group descriptor (64bit feature).
type groupDesc struct {
	BlockBitmapLo     uint32 // 0x00
	InodeBitmapLo     uint32 // 0x04
	InodeTableLo      uint32 // 0x08
	FreeBlockCountLo  uint16 // 0x0C
	FreeInodeCountLo  uint16 // 0x0E
	UsedDirsCountLo   uint16 // 0x10
	Flags             uint16 // 0x12
	ExcludeBitmapLo   uint32 // 0x14
	BlockBitmapCsumLo uint16 // 0x18
	InodeBitmapCsumLo uint16 // 0x1A
	ItableUnusedLo    uint16 // 0x1C
	Checksum          uint16 // 0x1E
	BlockBitmapHi     uint32 // 0x20
	InodeBitmapHi     uint32 // 0x24
	InodeTableHi      uint32 // 0x28
	FreeBlockCountHi  uint16 // 0x2C
	FreeInodeCountHi  uint16 // 0x2E
	UsedDirsCountHi   uint16 // 0x30
	ItableUnusedHi    uint16 // 0x32
	ExcludeBitmapHi   uint32 // 0x34
	BlockBitmapCsumHi uint16 // 0x38
	InodeBitmapCsumHi uint16 // 0x3A
	Reserved          uint32 // 0x3C
}

// inode represents a 256-byte ext4 inode.
type inode struct {
	Mode        uint16    // 0x00
	UIDLo       uint16    // 0x02
	SizeLo      uint32    // 0x04
	Atime       uint32    // 0x08
	Ctime       uint32    // 0x0C
	Mtime       uint32    // 0x10
	Dtime       uint32    // 0x14
	GIDLo       uint16    // 0x18
	LinksCount  uint16    // 0x1A
	BlocksLo    uint32    // 0x1C  (in 512-byte units)
	Flags       uint32    // 0x20
	OSD1        uint32    // 0x24
	Block       [60]byte  // 0x28  (extent tree or inline data)
	Generation  uint32    // 0x64
	FileACLLo   uint32    // 0x68
	SizeHi      uint32    // 0x6C
	ObsoFaddr   uint32    // 0x70
	OSD2        [12]byte  // 0x74
	ExtraIsize  uint16    // 0x80
	ChecksumHi  uint16    // 0x82
	CtimeExtra  uint32    // 0x84
	MtimeExtra  uint32    // 0x88
	AtimeExtra  uint32    // 0x8C
	Crtime      uint32    // 0x90
	CrtimeExtra uint32    // 0x94
	VersionHi   uint32    // 0x98
	Projid      uint32    // 0x9C
	Padding     [100]byte // 0xA0-0xFF
}

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// popcount8Zero[b] = number of zero bits in byte b
var popcount8Zero [256]uint8

func init() {
	for i := 0; i < 256; i++ {
		popcount8Zero[i] = 8
		v := i
		for v != 0 {
			popcount8Zero[i]--
			v &= v - 1
		}
	}
}

// crc32cLE computes CRC32C matching ext2fs_crc32c_le semantics: the raw CRC
// without final XOR. Go's crc32.Update applies initial and final XOR
// automatically, so we invert to cancel them out.
func crc32cLE(crc uint32, data []byte) uint32 {
	return ^crc32.Update(^crc, crc32cTable, data)
}

// dirNode represents a directory in the tree being created.
// Each directory gets one inode and one data block.
type dirNode struct {
	name     string
	mode     uint32 // unix permission bits (e.g. 0755)
	uid      uint32
	gid      uint32
	ino      uint32 // allocated inode number
	block    uint32 // allocated data block number
	parent   *dirNode
	children []*dirNode
}

// formatParams holds computed parameters for the filesystem.
type formatParams struct {
	blockCount     uint32
	numGroups      uint32
	inodesPerGroup uint32
	totalInodes    uint32
	inodeTableBlks uint32 // per group
	gdtBlocks      uint32
	freeBlocks     uint32
	freeInodes     uint32
	overhead       uint32
	uuid           [16]byte
	hashSeed       [4]uint32
	checksumSeed   uint32
	now            uint32

	// Block layout: with flex_bg, all group metadata is packed
	// contiguously in group 0 after the superblock + GDT.
	bbmStart     uint32 // first block bitmap block
	ibmStart     uint32 // first inode bitmap block
	itableStart  uint32 // first inode table block
	rootDirBlock uint32
	lpfStart     uint32
	lpfBlocks    uint32

	// User directory tree
	userDirs     []*dirNode // flat list of all user dirs
	usedInodes   uint32     // total used inodes (11 + user dirs)
	usedDirCount uint32     // total directory count (root + lost+found + user dirs)
}

// buildDirTree builds a tree of dirNode from a flat list of dirEntry paths.
// It allocates inode numbers starting at nextIno and data blocks starting
// at nextBlock. Intermediate parent directories are created automatically
// with the same permissions and ownership as the child that first requires them.
func buildDirTree(dirs []dirEntry, nextIno, nextBlock uint32) ([]*dirNode, uint32, uint32) {
	nodes := make(map[string]*dirNode)
	var allNodes []*dirNode

	for _, d := range dirs {
		parts := splitPath(d.Path)
		for i, part := range parts {
			fullPath := strings.Join(parts[:i+1], "/")
			if _, exists := nodes[fullPath]; exists {
				// Update perms if this is an explicitly requested entry
				if i == len(parts)-1 {
					n := nodes[fullPath]
					n.mode = uint32(d.Mode & 0o7777)
					n.uid = d.UID
					n.gid = d.GID
				}
				continue
			}

			// New node: inherit the leaf entry's perms for both
			// the leaf itself and any intermediate parents it creates
			n := &dirNode{
				name:  part,
				mode:  uint32(d.Mode & 0o7777),
				uid:   d.UID,
				gid:   d.GID,
				ino:   nextIno,
				block: nextBlock,
			}
			nextIno++
			nextBlock++

			if i > 0 {
				parentPath := strings.Join(parts[:i], "/")
				n.parent = nodes[parentPath]
				n.parent.children = append(n.parent.children, n)
			}

			nodes[fullPath] = n
			allNodes = append(allNodes, n)
		}
	}

	return allNodes, nextIno, nextBlock
}

func computeParams(size int64, dirs []dirEntry) formatParams {
	var p formatParams

	p.blockCount = uint32(size / blockSize)
	p.numGroups = (p.blockCount + blocksPerGroup - 1) / blocksPerGroup

	// Inode ratio: match mkfs.ext4 behavior
	inodeRatio := uint32(16384)
	if p.blockCount <= 65536 {
		inodeRatio = 4096
	}

	totalInodes := uint64(size) / uint64(inodeRatio)
	p.inodesPerGroup = uint32(totalInodes) / p.numGroups
	// Round up to multiple of inodesPerBlock (16)
	if rem := p.inodesPerGroup % uint32(inodesPerBlock); rem != 0 {
		p.inodesPerGroup += uint32(inodesPerBlock) - rem
	}
	if p.inodesPerGroup < 16 {
		p.inodesPerGroup = 16
	}
	p.totalInodes = p.inodesPerGroup * p.numGroups
	p.inodeTableBlks = p.inodesPerGroup / uint32(inodesPerBlock)

	p.gdtBlocks = (p.numGroups*groupDescSize + blockSize - 1) / blockSize

	// With flex_bg, all metadata is packed contiguously in group 0:
	// [superblock][GDT][bbm0..bbmN][ibm0..ibmN][itable0..itableN][data]
	nextBlock := uint32(1) + p.gdtBlocks // after sb + gdt

	p.bbmStart = nextBlock
	nextBlock += p.numGroups

	if p.numGroups == 1 {
		// Match mkfs.ext4 single-group layout: bbm=2, ibm=18, itable=34
		p.ibmStart = p.bbmStart + 16
		p.itableStart = 34
	} else {
		// Multi-group: pack tightly
		p.ibmStart = nextBlock
		nextBlock = p.ibmStart + p.numGroups
		p.itableStart = nextBlock
	}
	nextBlock = p.itableStart + p.numGroups*p.inodeTableBlks

	// Data blocks after metadata
	p.lpfBlocks = 4
	if p.numGroups == 1 {
		// Single group: data goes between GDT and inode bitmap (blocks 3-7)
		p.rootDirBlock = p.bbmStart + 1 // block 3
		p.lpfStart = p.rootDirBlock + 1 // blocks 4-7
		nextBlock = p.lpfStart + p.lpfBlocks
	} else {
		p.rootDirBlock = nextBlock
		p.lpfStart = nextBlock + 1
		nextBlock = p.lpfStart + p.lpfBlocks
	}

	// Allocate inodes and data blocks for user directories
	// Inode 12 is the first available (1-10 reserved, 11 = lost+found)
	p.userDirs, _, _ = buildDirTree(dirs, 12, nextBlock)
	numUserDirs := uint32(len(p.userDirs))
	p.usedInodes = 11 + numUserDirs
	p.usedDirCount = 2 + numUserDirs // root + lost+found + user dirs

	// Compute overhead = metadata blocks (not counting data)
	metaBlocks := uint32(1) + p.gdtBlocks + // sb + gdt
		p.numGroups + // block bitmaps
		p.numGroups + // inode bitmaps
		p.numGroups*p.inodeTableBlks // inode tables
	p.overhead = metaBlocks

	usedDataBlocks := uint32(1) + p.lpfBlocks + numUserDirs // root + lpf + user dirs
	p.freeBlocks = p.blockCount - metaBlocks - usedDataBlocks
	p.freeInodes = p.totalInodes - p.usedInodes

	// Generate UUID and hash seed
	io.ReadFull(rand.Reader, p.uuid[:])
	p.uuid[6] = (p.uuid[6] & 0x0F) | 0x40
	p.uuid[8] = (p.uuid[8] & 0x3F) | 0x80

	var seedBytes [16]byte
	io.ReadFull(rand.Reader, seedBytes[:])
	for i := 0; i < 4; i++ {
		p.hashSeed[i] = binary.LittleEndian.Uint32(seedBytes[i*4:])
	}

	p.checksumSeed = crc32cLE(^uint32(0), p.uuid[:])
	p.now = uint32(time.Now().Unix())

	return p
}

// Create creates a new ext4 filesystem image at the given path with
// the specified size in bytes (minimum 64 MiB). It writes the structures
// directly without requiring external tools.
func Create(path string, size int64, opts ...CreateOpt) error {
	var cfg createConfig
	for _, o := range opts {
		o(&cfg)
	}

	if size <= 0 {
		return fmt.Errorf("ext4: size must be positive, got %d", size)
	}
	if size < 64*1024*1024 {
		return fmt.Errorf("ext4: minimum size is 64 MiB, got %d", size)
	}

	p := computeParams(size, cfg.dirs)

	if p.itableStart+p.numGroups*p.inodeTableBlks > p.blockCount {
		return fmt.Errorf("ext4: filesystem too small for metadata")
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("ext4: create file: %w", err)
	}
	defer f.Close()

	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("ext4: truncate: %w", err)
	}

	// Pre-build bitmaps (shared between GDT checksums and disk writes)
	bbmBufs := make([][]byte, p.numGroups)
	ibmBufs := make([][]byte, p.numGroups)
	for g := uint32(0); g < p.numGroups; g++ {
		bbmBufs[g] = buildBlockBitmap(&p, g)
		ibmBufs[g] = buildInodeBitmap(&p, g)
	}

	sb := buildSuperblock(&p)
	if err := writeSuperblock(f, &sb); err != nil {
		return err
	}

	if err := writeGroupDescs(f, &p, &sb, bbmBufs, ibmBufs); err != nil {
		return err
	}

	if err := writeBitmaps(f, &p, bbmBufs, ibmBufs); err != nil {
		return err
	}

	if err := writeRootDir(f, &p, &sb); err != nil {
		return err
	}

	if err := writeEmptyDir(f, &p, &sb); err != nil {
		return err
	}

	for _, d := range p.userDirs {
		if err := writeUserDir(f, &p, &sb, d); err != nil {
			return err
		}
	}
	return nil
}

func buildSuperblock(p *formatParams) superblock {
	sb := superblock{
		InodeCount:       p.totalInodes,
		BlockCountLo:     p.blockCount,
		FreeBlockCountLo: p.freeBlocks,
		FreeInodeCount:   p.freeInodes,
		FirstDataBlock:   0,
		LogBlockSize:     logBlockSize,
		LogClusterSize:   logBlockSize,
		BlocksPerGroup:   blocksPerGroup,
		ClustersPerGroup: blocksPerGroup,
		InodesPerGroup:   p.inodesPerGroup,
		Wtime:            p.now,
		MaxMntCount:      0xFFFF,
		Magic:            ext4Magic,
		State:            1, // clean
		Errors:           1, // continue
		Lastcheck:        p.now,
		CreatorOS:        0, // Linux
		RevLevel:         1, // dynamic
		FirstIno:         11,
		InodeSize:        inodeSize,
		FeatureCompat:    compatExtAttr | compatDirIndex | compatSparseSuper,
		FeatureIncompat:  incompatFiletype | incompatExtents | incompat64bit | incompatFlexBG | incompatCsumSeed,
		FeatureRoCompat:  roCompatSparseSuper | roCompatLargeFile | roCompatHugeFile | roCompatDirNlink | roCompatExtraIsize | roCompatMetadataCsum,
		UUID:             p.uuid,
		HashSeed:         p.hashSeed,
		DefHashVersion:   hashHalfMD4,
		DescSize:         groupDescSize,
		DefaultMountOpts: defmXattrUser | defmACL,
		MkfsTime:         p.now,
		MinExtraIsize:    32,
		WantExtraIsize:   32,
		Flags:            flagsSignedHash,
		LogGroupsPerFlex: 4, // flex_bg size = 2^4 = 16
		ChecksumType:     1, // crc32c
		OverheadClusters: p.overhead,
		ChecksumSeed:     p.checksumSeed,
	}

	// Compute checksum over superblock (set checksum field to 0 first)
	sb.Checksum = 0
	sb.Checksum = sbChecksum(&sb)

	return sb
}

func sbChecksum(sb *superblock) uint32 {
	buf := make([]byte, superblockSize)
	encodeStruct(buf, sb)
	// e2fsprogs computes crc32c_le(~0, sb, 0x3FC) — over the first 1020
	// bytes only (everything before the checksum field).
	return crc32cLE(^uint32(0), buf[:0x3FC])
}

// encodeStruct serializes a struct into a byte slice using binary.LittleEndian.
func encodeStruct(buf []byte, v any) {
	w := &bufWriter{buf: buf}
	binary.Write(w, binary.LittleEndian, v)
}

// bufWriter is an io.Writer that writes into a byte slice at an advancing offset.
type bufWriter struct {
	buf []byte
	off int
}

func (w *bufWriter) Write(p []byte) (int, error) {
	n := copy(w.buf[w.off:], p)
	w.off += n
	return n, nil
}

func writeSuperblock(f *os.File, sb *superblock) error {
	buf := make([]byte, superblockSize)
	encodeStruct(buf, sb)

	_, err := f.WriteAt(buf, superblockOffset)
	if err != nil {
		return fmt.Errorf("ext4: write superblock: %w", err)
	}
	return nil
}

func writeGroupDescs(f *os.File, p *formatParams, sb *superblock, bbmBufs, ibmBufs [][]byte) error {
	buf := make([]byte, p.gdtBlocks*blockSize)

	for g := uint32(0); g < p.numGroups; g++ {
		groupFreeBlocks := countZeroBits(bbmBufs[g], groupBlockCount(p, g))
		groupFreeInodes := countZeroBits(ibmBufs[g], p.inodesPerGroup)

		usedDirs := uint16(0)
		unusedInodes := groupFreeInodes
		flags := uint16(bgInodeZeroed)
		if g == 0 {
			usedDirs = uint16(p.usedDirCount)
			unusedInodes = p.inodesPerGroup - p.usedInodes
		} else {
			// Groups beyond 0 have no allocated inodes
			flags |= bgInodeUninit
			// Groups with no allocated blocks (all metadata is in group 0
			// due to flex_bg, so non-zero groups with all-free blocks are BLOCK_UNINIT)
			if groupFreeBlocks == groupBlockCount(p, g) {
				flags |= bgBlockUninit
			}
		}

		gd := groupDesc{
			BlockBitmapLo:    p.bbmStart + g,
			InodeBitmapLo:    p.ibmStart + g,
			InodeTableLo:     p.itableStart + g*p.inodeTableBlks,
			FreeBlockCountLo: uint16(groupFreeBlocks),
			FreeInodeCountLo: uint16(groupFreeInodes),
			UsedDirsCountLo:  usedDirs,
			Flags:            flags,
			ItableUnusedLo:   uint16(unusedInodes),
		}

		bbmCsum := bitmapChecksum(sb, bbmBufs[g][:blocksPerGroup/8])
		gd.BlockBitmapCsumLo = uint16(bbmCsum & 0xFFFF)
		gd.BlockBitmapCsumHi = uint16(bbmCsum >> 16)

		ibmCsumSize := p.inodesPerGroup / 8
		if flags&bgInodeUninit != 0 {
			// INODE_UNINIT: checksum is 0
			gd.InodeBitmapCsumLo = 0
			gd.InodeBitmapCsumHi = 0
		} else {
			ibmCsum := bitmapChecksum(sb, ibmBufs[g][:ibmCsumSize])
			gd.InodeBitmapCsumLo = uint16(ibmCsum & 0xFFFF)
			gd.InodeBitmapCsumHi = uint16(ibmCsum >> 16)
		}

		off := g * groupDescSize
		encodeStruct(buf[off:off+groupDescSize], &gd)

		gdCsum := gdChecksum(sb, g, buf[off:off+groupDescSize])
		binary.LittleEndian.PutUint16(buf[off+0x1E:], gdCsum)
	}

	_, err := f.WriteAt(buf, int64(1)*blockSize) // GDT at block 1
	if err != nil {
		return fmt.Errorf("ext4: write group descriptors: %w", err)
	}
	return nil
}

// groupBlockCount returns the number of blocks in the given group.
func groupBlockCount(p *formatParams, g uint32) uint32 {
	start := g * blocksPerGroup
	end := start + blocksPerGroup
	if end > p.blockCount {
		end = p.blockCount
	}
	return end - start
}

// countZeroBits counts the number of zero bits in the first n bits of buf.
func countZeroBits(buf []byte, n uint32) uint32 {
	count := uint32(0)
	// Count whole bytes
	fullBytes := n / 8
	for i := uint32(0); i < fullBytes; i++ {
		count += uint32(popcount8Zero[buf[i]])
	}
	// Count remaining bits
	for bit := uint32(0); bit < n%8; bit++ {
		if buf[fullBytes]&(1<<bit) == 0 {
			count++
		}
	}
	return count
}

func writeBitmaps(f *os.File, p *formatParams, bbmBufs, ibmBufs [][]byte) error {
	for g := uint32(0); g < p.numGroups; g++ {
		// Only write block bitmap if the group has allocated blocks
		hasAllocatedBlocks := g == 0
		if !hasAllocatedBlocks {
			groupEnd := (g + 1) * blocksPerGroup
			hasAllocatedBlocks = groupEnd > p.blockCount
		}

		if hasAllocatedBlocks {
			if _, err := f.WriteAt(bbmBufs[g], int64(p.bbmStart+g)*blockSize); err != nil {
				return fmt.Errorf("ext4: write block bitmap %d: %w", g, err)
			}
		}

		// Only write inode bitmap for group 0 (others are INODE_UNINIT)
		if g == 0 {
			if _, err := f.WriteAt(ibmBufs[g], int64(p.ibmStart+g)*blockSize); err != nil {
				return fmt.Errorf("ext4: write inode bitmap %d: %w", g, err)
			}
		}
	}
	return nil
}

func buildBlockBitmap(p *formatParams, group uint32) []byte {
	buf := make([]byte, blockSize)

	setBit := func(block uint32) {
		// block is absolute; convert to group-relative
		groupStart := group * blocksPerGroup
		if block >= groupStart && block < groupStart+blocksPerGroup {
			rel := block - groupStart
			buf[rel/8] |= 1 << (rel % 8)
		}
	}

	// All metadata is in group 0 (flex_bg consolidation)
	// Superblock
	setBit(0)
	// GDT
	for i := uint32(0); i < p.gdtBlocks; i++ {
		setBit(1 + i)
	}
	// Block bitmaps
	for i := uint32(0); i < p.numGroups; i++ {
		setBit(p.bbmStart + i)
	}
	// Inode bitmaps
	for i := uint32(0); i < p.numGroups; i++ {
		setBit(p.ibmStart + i)
	}
	// Inode tables
	for i := uint32(0); i < p.numGroups*p.inodeTableBlks; i++ {
		setBit(p.itableStart + i)
	}
	// Root dir data
	setBit(p.rootDirBlock)
	// Lost+found data
	for i := uint32(0); i < p.lpfBlocks; i++ {
		setBit(p.lpfStart + i)
	}
	// User directory data blocks
	for _, d := range p.userDirs {
		setBit(d.block)
	}

	// Mark blocks beyond the filesystem as used (partial last group)
	groupStart := group * blocksPerGroup
	groupEnd := groupStart + blocksPerGroup
	if p.blockCount < groupEnd {
		startRel := p.blockCount - groupStart
		startByte := startRel / 8
		if startRel%8 != 0 {
			for bit := startRel % 8; bit < 8; bit++ {
				buf[startByte] |= 1 << bit
			}
			startByte++
		}
		for i := startByte; i < blockSize; i++ {
			buf[i] = 0xFF
		}
	}

	return buf
}

func buildInodeBitmap(p *formatParams, group uint32) []byte {
	buf := make([]byte, blockSize)

	if group == 0 {
		buf[0] = 0xFF // inodes 1-8
		buf[1] = 0x07 // inodes 9-11
		for _, d := range p.userDirs {
			ino := d.ino - 1
			buf[ino/8] |= 1 << (ino % 8)
		}
	}

	// Padding: fill whole bytes beyond inodesPerGroup with 0xFF
	startByte := p.inodesPerGroup / 8
	if p.inodesPerGroup%8 != 0 {
		// Partial byte: set remaining bits
		for bit := p.inodesPerGroup % 8; bit < 8; bit++ {
			buf[startByte] |= 1 << bit
		}
		startByte++
	}
	for i := startByte; i < blockSize; i++ {
		buf[i] = 0xFF
	}

	return buf
}

func writeRootDir(f *os.File, p *formatParams, sb *superblock) error {
	// Count root-level children for link count
	rootChildren := uint16(0)
	for _, d := range p.userDirs {
		if d.parent == nil {
			rootChildren++
		}
	}

	ino := inode{
		Mode:       0o40755,
		SizeLo:     blockSize,
		Atime:      p.now,
		Ctime:      p.now,
		Mtime:      p.now,
		LinksCount: 3 + rootChildren, // ., .., lost+found, + each subdir
		BlocksLo:   blockSize / 512,
		Flags:      extentsFlag,
		ExtraIsize: 32,
		Crtime:     p.now,
	}
	writeExtent(ino.Block[:], 1, p.rootDirBlock)
	if err := writeInode(f, p, sb, 2, &ino); err != nil {
		return err
	}

	// Build root directory data block with entries
	dirBuf := make([]byte, blockSize)
	off := 0
	off += writedirEntryAt(dirBuf[off:], 2, ".", ftDir, 12)
	off += writedirEntryAt(dirBuf[off:], 2, "..", ftDir, 12)

	// Collect root-level entries: lost+found + user dirs at root
	type dirEnt struct {
		ino  uint32
		name string
	}
	entries := []dirEnt{{11, "lost+found"}}
	for _, d := range p.userDirs {
		if d.parent == nil {
			entries = append(entries, dirEnt{d.ino, d.name})
		}
	}

	for i, e := range entries {
		recLen := dirEntryRecLen(e.name)
		if i == len(entries)-1 {
			// Last entry fills remaining space minus tail
			recLen = uint16(blockSize - off - 12)
		}
		off += writedirEntryAt(dirBuf[off:], e.ino, e.name, ftDir, recLen)
	}
	writeDirTail(dirBuf, sb, 2)

	_, err := f.WriteAt(dirBuf, int64(p.rootDirBlock)*blockSize)
	return err
}

// dirEntryRecLen returns the minimum record length for a dir entry with the given name.
// Must be a multiple of 4.
func dirEntryRecLen(name string) uint16 {
	// 8 bytes header + name length, rounded up to 4
	return uint16((8 + len(name) + 3) &^ 3)
}

func writeEmptyDir(f *os.File, p *formatParams, sb *superblock) error {
	ino := inode{
		Mode:       0o40700,
		SizeLo:     p.lpfBlocks * blockSize,
		Atime:      p.now,
		Ctime:      p.now,
		Mtime:      p.now,
		LinksCount: 2,
		BlocksLo:   p.lpfBlocks * (blockSize / 512),
		Flags:      extentsFlag,
		ExtraIsize: 32,
		Crtime:     p.now,
	}
	writeExtent(ino.Block[:], p.lpfBlocks, p.lpfStart)
	if err := writeInode(f, p, sb, 11, &ino); err != nil {
		return err
	}

	for b := uint32(0); b < p.lpfBlocks; b++ {
		dirBuf := make([]byte, blockSize)
		if b == 0 {
			off := 0
			off += writedirEntryAt(dirBuf[off:], 11, ".", ftDir, 12)
			remaining := blockSize - off - 12
			writedirEntryAt(dirBuf[off:], 2, "..", ftDir, uint16(remaining))
		} else {
			binary.LittleEndian.PutUint32(dirBuf[0:], 0)
			binary.LittleEndian.PutUint16(dirBuf[4:], uint16(blockSize-12))
		}
		writeDirTail(dirBuf, sb, 11)
		if _, err := f.WriteAt(dirBuf, int64(p.lpfStart+b)*blockSize); err != nil {
			return err
		}
	}
	return nil
}

func writeUserDir(f *os.File, p *formatParams, sb *superblock, d *dirNode) error {
	childDirs := uint16(len(d.children))

	parentIno := uint32(2) // root
	if d.parent != nil {
		parentIno = d.parent.ino
	}

	ino := inode{
		Mode:       0o40000 | uint16(d.mode),
		UIDLo:      uint16(d.uid),
		GIDLo:      uint16(d.gid),
		SizeLo:     blockSize,
		Atime:      p.now,
		Ctime:      p.now,
		Mtime:      p.now,
		LinksCount: 2 + childDirs, // . + .. + child dirs
		BlocksLo:   blockSize / 512,
		Flags:      extentsFlag,
		ExtraIsize: 32,
		Crtime:     p.now,
	}
	// High UID/GID bits
	ino.OSD2[4] = byte(d.uid >> 16)
	ino.OSD2[5] = byte(d.uid >> 24)
	ino.OSD2[6] = byte(d.gid >> 16)
	ino.OSD2[7] = byte(d.gid >> 24)

	writeExtent(ino.Block[:], 1, d.block)
	if err := writeInode(f, p, sb, d.ino, &ino); err != nil {
		return err
	}

	// Build directory data block
	dirBuf := make([]byte, blockSize)
	off := 0
	off += writedirEntryAt(dirBuf[off:], d.ino, ".", ftDir, 12)
	off += writedirEntryAt(dirBuf[off:], parentIno, "..", ftDir, 12)

	// Add child entries
	for i, c := range d.children {
		recLen := dirEntryRecLen(c.name)
		if i == len(d.children)-1 {
			recLen = uint16(blockSize - off - 12)
		}
		off += writedirEntryAt(dirBuf[off:], c.ino, c.name, ftDir, recLen)
	}

	// If no children, ".." gets the remaining space
	if len(d.children) == 0 {
		// Rewrite ".." with the remaining space
		off = 0
		off += writedirEntryAt(dirBuf[off:], d.ino, ".", ftDir, 12)
		remaining := uint16(blockSize - off - 12)
		writedirEntryAt(dirBuf[off:], parentIno, "..", ftDir, remaining)
	}

	writeDirTail(dirBuf, sb, d.ino)
	_, err := f.WriteAt(dirBuf, int64(d.block)*blockSize)
	return err
}

func writeExtent(block []byte, numBlocks uint32, physBlock uint32) {
	// Extent header
	binary.LittleEndian.PutUint16(block[0:], extentMagic)
	binary.LittleEndian.PutUint16(block[2:], 1)               // 1 entry
	binary.LittleEndian.PutUint16(block[4:], maxExtentLeaves) // max entries
	binary.LittleEndian.PutUint16(block[6:], 0)               // depth 0 (leaf)
	binary.LittleEndian.PutUint32(block[8:], 0)               // generation

	// Single extent entry at offset 12
	binary.LittleEndian.PutUint32(block[12:], 0)                 // logical block 0
	binary.LittleEndian.PutUint16(block[16:], uint16(numBlocks)) // length
	binary.LittleEndian.PutUint16(block[18:], 0)                 // phys high
	binary.LittleEndian.PutUint32(block[20:], physBlock)         // phys low
}

func writeInode(f *os.File, p *formatParams, sb *superblock, inodeNum uint32, ino *inode) error {
	inoCsum := inodeChecksum(sb, inodeNum, ino)
	binary.LittleEndian.PutUint16(ino.OSD2[8:], uint16(inoCsum&0xFFFF))
	ino.ChecksumHi = uint16(inoCsum >> 16)

	buf := make([]byte, inodeSize)
	encodeStruct(buf, ino)

	offset := int64(p.itableStart)*blockSize + int64(inodeNum-1)*inodeSize
	_, err := f.WriteAt(buf, offset)
	if err != nil {
		return fmt.Errorf("ext4: write inode %d: %w", inodeNum, err)
	}
	return nil
}

func writedirEntryAt(buf []byte, inodeNum uint32, name string, fileType uint8, recLen uint16) int {
	binary.LittleEndian.PutUint32(buf[0:], inodeNum)
	binary.LittleEndian.PutUint16(buf[4:], recLen)
	buf[6] = uint8(len(name))
	buf[7] = fileType
	copy(buf[8:], name)
	return int(recLen)
}

func writeDirTail(buf []byte, sb *superblock, inodeNum uint32) {
	tailOff := blockSize - 12
	binary.LittleEndian.PutUint32(buf[tailOff:], 0)    // reserved_zero1
	binary.LittleEndian.PutUint16(buf[tailOff+4:], 12) // rec_len
	buf[tailOff+6] = 0                                 // reserved_zero2
	buf[tailOff+7] = 0xDE                              // reserved_ft (magic)

	// Checksum of directory block
	csum := dirBlockChecksum(sb, inodeNum, buf[:blockSize])
	binary.LittleEndian.PutUint32(buf[tailOff+8:], csum)
}

// Checksum functions

func dirBlockChecksum(sb *superblock, inodeNum uint32, block []byte) uint32 {
	seed := sb.ChecksumSeed
	var ibuf [8]byte
	binary.LittleEndian.PutUint32(ibuf[0:], inodeNum)
	binary.LittleEndian.PutUint32(ibuf[4:], 0) // generation = 0
	csum := crc32cLE(seed, ibuf[:])
	// CRC over directory entries only (block minus 12-byte tail)
	csum = crc32cLE(csum, block[:blockSize-12])
	return csum
}

func inodeChecksum(sb *superblock, inodeNum uint32, ino *inode) uint32 {
	seed := sb.ChecksumSeed

	var nbuf [4]byte
	binary.LittleEndian.PutUint32(nbuf[:], inodeNum)
	csum := crc32cLE(seed, nbuf[:])

	var gbuf [4]byte
	binary.LittleEndian.PutUint32(gbuf[:], ino.Generation)
	csum = crc32cLE(csum, gbuf[:])

	// Serialize inode with checksum fields zeroed
	buf := make([]byte, inodeSize)
	savedCsumLo := binary.LittleEndian.Uint16(ino.OSD2[8:])
	savedCsumHi := ino.ChecksumHi
	binary.LittleEndian.PutUint16(ino.OSD2[8:], 0)
	ino.ChecksumHi = 0
	encodeStruct(buf, ino)
	binary.LittleEndian.PutUint16(ino.OSD2[8:], savedCsumLo)
	ino.ChecksumHi = savedCsumHi

	csum = crc32cLE(csum, buf)
	return csum
}

func bitmapChecksum(sb *superblock, bitmap []byte) uint32 {
	return crc32cLE(sb.ChecksumSeed, bitmap)
}

func gdChecksum(sb *superblock, group uint32, desc []byte) uint16 {
	seed := sb.ChecksumSeed
	var gbuf [4]byte
	binary.LittleEndian.PutUint32(gbuf[:], group)
	csum := crc32cLE(seed, gbuf[:])

	// Checksum the descriptor, skipping the 2-byte checksum field at 0x1E
	csum = crc32cLE(csum, desc[:0x1E])
	csum = crc32cLE(csum, make([]byte, 2))
	csum = crc32cLE(csum, desc[0x20:])

	return uint16(csum & 0xFFFF)
}
