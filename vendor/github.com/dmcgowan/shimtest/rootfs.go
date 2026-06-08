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
	"bytes"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/continuity/pkg/ext4"
	erofs "github.com/erofs/go-erofs"
)

const shimtestModulePath = "github.com/dmcgowan/shimtest"

// testbinOS is the OS for which testbin is always built. Regardless of the
// host running the tests, the container binary is always a Linux ELF.
const testbinOS = "linux"

// testbinLocalPath is the path checked first for a locally built testbin,
// relative to the module source root. It lives under testdata/ so that
// go mod vendor never copies it into downstream vendor trees.
//
//go:generate make testbin
const testbinLocalPath = "testdata/testbin"

// openTestbin returns a reader for the testbin binary. It checks
// testdata/testbin first (populated by "make testbin" or "go generate"),
// then falls back to downloading the binary for the current GOARCH from the
// GitHub release for testbinVersion. The downloaded binary is cached at
// testdata/testbin for subsequent runs.
//
// Set SHIMTEST_TESTBIN to the path of a pre-built binary to skip both.
func openTestbin() (io.Reader, error) {
	// Explicit override.
	if path := os.Getenv("SHIMTEST_TESTBIN"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("SHIMTEST_TESTBIN: %w", err)
		}
		return bytes.NewReader(data), nil
	}

	// Local build: testdata/testbin (populated by make testbin / go generate).
	if data, err := os.ReadFile(testbinLocalPath); err == nil && len(data) > 0 {
		return bytes.NewReader(data), nil
	}

	// Fetch from the GitHub release for the current testbinVersion.
	data, err := fetchTestbin(testbinVersion, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

// fetchTestbin downloads the testbin Linux binary for the given version/goarch
// from the GitHub release, caching it at testdata/testbin for subsequent runs.
func fetchTestbin(version, goarch string) ([]byte, error) {
	url := testbinURL(version, goarch)
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("shimtest: fetching testbin from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"shimtest: fetching testbin from %s: unexpected status %s; "+
				"ensure a release exists for version %s, run 'make testbin', or set SHIMTEST_TESTBIN",
			url, resp.Status, version,
		)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("shimtest: reading testbin from %s: %w", url, err)
	}

	// Cache at testdata/testbin so subsequent runs skip the download.
	if err := os.MkdirAll("testdata", 0755); err == nil {
		_ = os.WriteFile(testbinLocalPath, data, 0755)
	}
	return data, nil
}

func testbinURL(version, goarch string) string {
	return fmt.Sprintf(
		"https://github.com/%s/releases/download/%s/%s",
		strings.TrimPrefix(shimtestModulePath, "github.com/"),
		version,
		testbinAssetName(goarch),
	)
}

func testbinAssetName(goarch string) string {
	return fmt.Sprintf("testbin-%s-%s", testbinOS, goarch)
}

// testbinCommands lists the commands provided by the testbin binary.
// Symlinks are created in /bin for each command in the embedded
// rootfs.
var testbinCommands = []string{"forever", "burstexit", "cat", "date", "echo", "exit", "hashverify", "layercheck", "ls", "memhog", "nc", "tickexit"}

// bigFileSize is the size of the IO benchmark fixture file. Large
// enough to swamp small per-call overheads while still building /
// extracting in well under a second on host storage.
const bigFileSize = 64 << 20 // 64 MiB

// bigFileContainerPath is where the IO fixture file is mounted
// inside the container, both via the dedicated erofs layer and via
// host bind mount (in different tests).
const bigFileContainerPath = "/data/bigfile"

// statDirContainerPath is a small directory in the secondary erofs
// layer used as the target for stress stat operations. Transferring
// it with NoWalk=true produces a tar archive containing only the
// directory header — the cheapest possible bidirectional-stream
// payload.
const statDirContainerPath = "/data/stat"

// statFileContainerPath is a small fixed file inside statDirContainerPath.
const statFileContainerPath = "/data/stat/probe"

// bigFileSeed is the ChaCha8 seed used to generate the fixture
// payload. Any change here invalidates the cached hash.
var bigFileSeed = [32]byte{
	's', 'h', 'i', 'm', 't', 'e', 's', 't',
	'b', 'i', 'g', 'f', 'i', 'l', 'e', 'v', '1',
}

// bigFileReader streams deterministic ChaCha8-seeded bytes up to
// bigFileSize and computes the crc32-Castagnoli of what it produced.
type bigFileReader struct {
	src    *rand.ChaCha8
	crc    hash.Hash32
	remain int
}

// newBigFileReader returns a fresh reader for the canonical bigfile
// fixture (bigFileSize bytes, deterministic). Use bigFileHashHex to
// obtain the corresponding hash.
func newBigFileReader() *bigFileReader {
	return &bigFileReader{
		src:    rand.NewChaCha8(bigFileSeed),
		crc:    crc32.New(crc32.MakeTable(crc32.Castagnoli)),
		remain: bigFileSize,
	}
}

// Read implements io.Reader.
func (r *bigFileReader) Read(p []byte) (int, error) {
	if r.remain == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remain {
		n = r.remain
	}
	nr, _ := r.src.Read(p[:n])
	r.crc.Write(p[:nr])
	r.remain -= nr
	return nr, nil
}

// hashHex returns the crc32-Castagnoli (hex) of bytes produced so
// far. Call after the reader is fully consumed.
func (r *bigFileReader) hashHex() string {
	return fmt.Sprintf("%08x", r.crc.Sum32())
}

// bigFileHashHex returns the canonical crc32-Castagnoli of the
// fixture by streaming through io.Discard.
func bigFileHashHex() string {
	r := newBigFileReader()
	_, _ = io.Copy(io.Discard, r)
	return r.hashHex()
}

// writeBigFileErofs builds a separate erofs image containing the IO
// benchmark fixture mounted at /data/bigfile and the stress stat
// fixture at /data/stat/probe. Returns the image path; callers
// compose it as an additional lower in the rootfs overlay.
func writeBigFileErofs(tb testing.TB) string {
	tb.Helper()

	// Use the same dir for the image and the erofs spool file so that
	// t.TempDir()'s cleanup removes both. Without WithTempDir, the spool
	// lands in os.TempDir() and the Unix unlink-while-open trick that
	// go-erofs relies on fails silently on Windows, leaving files behind.
	dir := tb.TempDir()
	imgPath := filepath.Join(dir, "bigfile.erofs")
	f, err := os.Create(imgPath)
	if err != nil {
		tb.Fatal("create bigfile erofs:", err)
	}
	defer f.Close()

	w := erofs.Create(f, erofs.WithTempDir(dir))

	if err := w.Mkdir("data", 0755); err != nil {
		tb.Fatal("mkdir data:", err)
	}

	bf, err := w.Create("data/bigfile")
	if err != nil {
		tb.Fatal("create bigfile entry:", err)
	}
	if _, err := io.Copy(bf, newBigFileReader()); err != nil {
		tb.Fatal("write bigfile:", err)
	}
	if err := bf.Chmod(0644); err != nil {
		tb.Fatal("chmod bigfile:", err)
	}
	if err := bf.Close(); err != nil {
		tb.Fatal("close bigfile:", err)
	}

	// Stress stat fixture: a small directory containing a single file.
	if err := w.Mkdir("data/stat", 0755); err != nil {
		tb.Fatal("mkdir data/stat:", err)
	}
	pf, err := w.Create("data/stat/probe")
	if err != nil {
		tb.Fatal("create probe entry:", err)
	}
	if _, err := pf.Write([]byte("probe\n")); err != nil {
		tb.Fatal("write probe:", err)
	}
	if err := pf.Chmod(0644); err != nil {
		tb.Fatal("chmod probe:", err)
	}
	if err := pf.Close(); err != nil {
		tb.Fatal("close probe:", err)
	}

	if err := w.Close(); err != nil {
		tb.Fatal("erofs close:", err)
	}

	return imgPath
}

// writeRootfsErofs builds an erofs image containing the testbin
// rootfs directly from the embedded binary, without touching the
// local filesystem. Returns the path to the image file.
func writeRootfsErofs(tb testing.TB) string {
	tb.Helper()

	// Use the same dir for the image and the erofs spool file so that
	// t.TempDir()'s cleanup removes both. Without WithTempDir, the spool
	// lands in os.TempDir() and the Unix unlink-while-open trick that
	// go-erofs relies on fails silently on Windows, leaving files behind.
	dir := tb.TempDir()
	imgPath := filepath.Join(dir, "rootfs.erofs")
	f, err := os.Create(imgPath)
	if err != nil {
		tb.Fatal("create erofs image:", err)
	}
	defer f.Close()

	w := erofs.Create(f, erofs.WithTempDir(dir))

	for _, d := range []string{"bin", "dev", "etc", "proc", "run", "sys", "tmp"} {
		if err := w.Mkdir(d, 0755); err != nil {
			tb.Fatal("mkdir:", err)
		}
	}

	bin, err := w.Create("bin/testbin")
	if err != nil {
		tb.Fatal("create testbin:", err)
	}
	r, err := openTestbin()
	if err != nil {
		tb.Fatal("open testbin:", err)
	}
	if _, err := io.Copy(bin, r); err != nil {
		tb.Fatal("write testbin:", err)
	}
	if err := bin.Chmod(0755); err != nil {
		tb.Fatal("chmod testbin:", err)
	}
	if err := bin.Close(); err != nil {
		tb.Fatal("close testbin:", err)
	}

	for _, cmd := range testbinCommands {
		if err := w.Symlink("testbin", "bin/"+cmd); err != nil {
			tb.Fatal("symlink:", err)
		}
	}

	writeErofsFile(tb, w, "etc/passwd", []byte("root:x:0:0:root:/root:/bin/forever\n"))
	writeErofsFile(tb, w, "etc/group", []byte("root:x:0:\n"))
	writeErofsFile(tb, w, "etc/resolv.conf", []byte(""))
	writeErofsFile(tb, w, "etc/hostname", []byte("shimtest\n"))
	writeErofsFile(tb, w, "etc/hosts", []byte("127.0.0.1\tlocalhost\n"))

	if err := w.Close(); err != nil {
		tb.Fatal("erofs close:", err)
	}

	return imgPath
}

// writeErofsFile is a helper to create a small file in an erofs image.
func writeErofsFile(tb testing.TB, w *erofs.Writer, path string, data []byte) {
	tb.Helper()
	f, err := w.Create(path)
	if err != nil {
		tb.Fatalf("create %s: %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		tb.Fatalf("write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		tb.Fatalf("close %s: %v", path, err)
	}
}

// extractErofsIntoDir opens an erofs image and copies its contents
// into dir.
func extractErofsIntoDir(tb testing.TB, imgPath, dir string) {
	tb.Helper()

	f, err := os.Open(imgPath)
	if err != nil {
		tb.Fatal("open erofs:", err)
	}
	defer f.Close()

	img, err := erofs.Open(f)
	if err != nil {
		tb.Fatal("erofs open:", err)
	}

	type readLinker interface {
		ReadLink(string) (string, error)
	}
	rl, hasReadLink := img.(readLinker)

	err = fs.WalkDir(img, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, path)

		if d.Type()&fs.ModeSymlink != 0 {
			if !hasReadLink {
				return &os.PathError{Op: "readlink", Path: path, Err: os.ErrInvalid}
			}
			linkTarget, err := rl.ReadLink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		}

		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		src, err := img.(fs.FS).Open(path)
		if err != nil {
			return err
		}
		defer src.Close()

		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer dst.Close()

		_, err = io.Copy(dst, src)
		return err
	})
	if err != nil {
		tb.Fatal("extract erofs:", err)
	}
}

// extractErofsToDir extracts an erofs image into a new temp directory
// and returns its path.
func extractErofsToDir(tb testing.TB, imgPath string) string {
	tb.Helper()
	dir := filepath.Join(tb.TempDir(), "rootfs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		tb.Fatal("mkdir:", err)
	}
	extractErofsIntoDir(tb, imgPath, dir)
	return dir
}

// buildEmbeddedRootfs builds rootfs mounts from the embedded testbin
// binary. When cfg.FormatMounts is set, the rootfs is provided as
// erofs and ext4 images with a format/mkdir/overlay mount descriptor
// for the shim to mount. When running as root, the rootfs is
// extracted and provided as an overlay mount. When running as
// non-root (rootless), the rootfs is extracted directly into the
// bundle's rootfs directory with no mounts.
func buildEmbeddedRootfs(tb testing.TB, bundleDir string, cfg Config) []*types.Mount {
	tb.Helper()

	erofsImg := writeRootfsErofs(tb)
	bigImg := writeBigFileErofs(tb)

	if cfg.FormatMounts {
		return buildErofsMounts(tb, []string{erofsImg, bigImg})
	}

	if os.Getuid() != 0 {
		rootfs := filepath.Join(bundleDir, "rootfs")
		extractErofsIntoDir(tb, erofsImg, rootfs)
		extractErofsIntoDir(tb, bigImg, rootfs)
		return nil
	}

	srcDir := extractErofsToDir(tb, erofsImg)
	extractErofsIntoDir(tb, bigImg, srcDir)
	return buildOverlayMounts(tb, srcDir)
}

// buildOverlayMounts returns an overlay mount with lower (read-only
// rootfs), upper (writable), and work directories.
func buildOverlayMounts(tb testing.TB, lower string) []*types.Mount {
	tb.Helper()

	dir := tb.TempDir()
	upper := filepath.Join(dir, "upper")
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(upper, 0755); err != nil {
		tb.Fatal("mkdir upper:", err)
	}
	if err := os.MkdirAll(work, 0755); err != nil {
		tb.Fatal("mkdir work:", err)
	}

	return []*types.Mount{{
		Type:   "overlay",
		Source: "overlay",
		Options: []string{
			"index=off",
			"workdir=" + work,
			"upperdir=" + upper,
			"lowerdir=" + lower,
		},
	}}
}

// overlayWhiteoutMode is the unix mode for an overlayfs whiteout
// entry: a character device with type bits S_IFCHR (0o20000) and
// permission 0. Combined with rdev = 0 (major:minor 0:0), this is
// what the kernel overlayfs driver recognizes as a whiteout in any
// lower layer.
const overlayWhiteoutMode = 0o20000

// writeLayersErofs builds n erofs layer images that stack on top of
// the testbin rootfs (writeRootfsErofs) to exercise multi-layer
// overlay handling. The structure is:
//
//   - Layer 1 (the bottom of the n new layers): seeds /base with
//     n-1 regular files (base_0 .. base_{n-2}, each containing
//     "base J\n") and adds /added/file_1 ("layer 1\n").
//   - Layers 2..n: each adds /added/file_K ("layer K\n") and writes
//     an overlayfs whiteout for /base/base_{K-2} (a char device with
//     rdev 0:0 — the standard "remove this from lower layers" marker).
//
// After all n layers are stacked above the rootfs, the visible
// filesystem contains all n /added/file_K files and an empty /base.
//
// The returned slice is ordered top-first so it can be passed
// directly as the erofsImgs argument to buildErofsMounts: index 0 is
// layer n (highest priority, applied last), index n-1 is layer 1
// (lowest priority, applied first).
func writeLayersErofs(tb testing.TB, n int) []string {
	tb.Helper()
	if n < 2 {
		tb.Fatalf("writeLayersErofs: n=%d must be >= 2", n)
	}

	dir := tb.TempDir()
	paths := make([]string, n)

	// Layer 1: seeds /base with n-1 files and adds /added/file_1.
	{
		imgPath := filepath.Join(dir, "layer-001.erofs")
		f, err := os.Create(imgPath)
		if err != nil {
			tb.Fatal("create layer 1 erofs:", err)
		}

		w := erofs.Create(f)

		if err := w.Mkdir("base", 0755); err != nil {
			tb.Fatal("mkdir base:", err)
		}
		for k := 0; k < n-1; k++ {
			writeErofsFile(tb, w, fmt.Sprintf("base/base_%d", k), []byte(fmt.Sprintf("base %d\n", k)))
		}

		if err := w.Mkdir("added", 0755); err != nil {
			tb.Fatal("mkdir added:", err)
		}
		writeErofsFile(tb, w, "added/file_1", []byte("layer 1\n"))

		if err := w.Close(); err != nil {
			tb.Fatal("close layer 1 erofs:", err)
		}
		if err := f.Close(); err != nil {
			tb.Fatal("close layer 1 file:", err)
		}
		// Layer 1 is the bottommost lower → last entry (lowest priority).
		paths[n-1] = imgPath
	}

	// Layers 2..n: each adds one file to /added and whites out one
	// file from /base. Layer K removes base_{K-2} so that across
	// layers 2..n the whiteouts cover base_0..base_{n-2}.
	for k := 2; k <= n; k++ {
		imgPath := filepath.Join(dir, fmt.Sprintf("layer-%03d.erofs", k))
		f, err := os.Create(imgPath)
		if err != nil {
			tb.Fatalf("create layer %d erofs: %v", k, err)
		}

		w := erofs.Create(f)

		if err := w.Mkdir("added", 0755); err != nil {
			tb.Fatalf("mkdir added (layer %d): %v", k, err)
		}
		writeErofsFile(tb, w, fmt.Sprintf("added/file_%d", k), []byte(fmt.Sprintf("layer %d\n", k)))

		if err := w.Mkdir("base", 0755); err != nil {
			tb.Fatalf("mkdir base (layer %d): %v", k, err)
		}
		// Overlayfs whiteout: char device 0:0. mknod with type bits
		// S_IFCHR and rdev = 0 makes the kernel hide base_{k-2} in
		// every lower layer below this one.
		if err := w.Mknod(fmt.Sprintf("base/base_%d", k-2), overlayWhiteoutMode, 0); err != nil {
			tb.Fatalf("mknod whiteout (layer %d): %v", k, err)
		}

		if err := w.Close(); err != nil {
			tb.Fatalf("close layer %d erofs: %v", k, err)
		}
		if err := f.Close(); err != nil {
			tb.Fatalf("close layer %d file: %v", k, err)
		}
		// Top-first ordering: higher k = newer layer = higher
		// priority = earlier in slice. Layer n at index 0, layer 2
		// at index n-2.
		paths[n-k] = imgPath
	}

	return paths
}

// shimImages holds the pre-built read-only erofs images needed by
// shimSetup. Benchmarks build these once before their iteration loop
// (with buildShimImages) and produce per-iteration writable mounts
// from them (with buildRootfsMountsFromImages), avoiding the O(b.N)
// accumulation of large erofs files on the tmpfs.
type shimImages struct {
	erofsImg string // rootfs.erofs path
	bigImg   string // bigfile.erofs path
	lowerDir string // pre-extracted overlay lower dir (root, !FormatMounts only)
}

// buildShimImages pre-builds the constant read-only rootfs images that
// buildEmbeddedRootfs would otherwise create on every shimSetup call.
// Intended for benchmarks: call once before the b.N loop and pass the
// result to buildRootfsMountsFromImages on each iteration.
func buildShimImages(tb testing.TB, cfg Config) shimImages {
	tb.Helper()
	var imgs shimImages
	imgs.erofsImg = writeRootfsErofs(tb)
	imgs.bigImg = writeBigFileErofs(tb)
	// For root overlay mode, pre-extract the lower directory once so
	// per-iteration setup only needs to create upper/work.
	if !cfg.FormatMounts && os.Getuid() == 0 {
		imgs.lowerDir = extractErofsToDir(tb, imgs.erofsImg)
		extractErofsIntoDir(tb, imgs.bigImg, imgs.lowerDir)
	}
	return imgs
}

// buildRootfsMountsFromImages is the per-iteration complement of
// buildShimImages. It creates only the writable parts of the rootfs
// (ext4 scratch image for FormatMounts, or overlay upper/work
// directories otherwise) fresh for each benchmark iteration.
// rootfsDir is the bundle's rootfs directory (used for rootless mode).
func buildRootfsMountsFromImages(tb testing.TB, cfg Config, imgs shimImages, rootfsDir string) []*types.Mount {
	tb.Helper()
	if cfg.FormatMounts {
		return buildErofsMounts(tb, []string{imgs.erofsImg, imgs.bigImg})
	}
	if os.Getuid() != 0 {
		extractErofsIntoDir(tb, imgs.erofsImg, rootfsDir)
		extractErofsIntoDir(tb, imgs.bigImg, rootfsDir)
		return nil
	}
	return buildOverlayMounts(tb, imgs.lowerDir)
}

// buildErofsMounts builds the layered erofs layout: ext4 (rw scratch)
// + one or more erofs lowers + overlay. Lowers are stacked in order,
// with erofsImgs[0] highest (matching overlay's lowerdir semantics).
func buildErofsMounts(tb testing.TB, erofsImgs []string) []*types.Mount {
	tb.Helper()

	ext4Img := filepath.Join(tb.TempDir(), "scratch.ext4")
	if err := ext4.Create(ext4Img, 64*1024*1024); err != nil {
		tb.Fatal("create ext4:", err)
	}

	mounts := []*types.Mount{{
		Type:    "ext4",
		Source:  ext4Img,
		Options: []string{"rw", "loop"},
	}}

	lowers := make([]string, 0, len(erofsImgs))
	for i, img := range erofsImgs {
		mounts = append(mounts, &types.Mount{
			Type:    "erofs",
			Source:  img,
			Options: []string{"ro", "loop"},
		})
		lowers = append(lowers, fmt.Sprintf("{{ mount %d }}", i+1))
	}

	mounts = append(mounts, &types.Mount{
		Type:   "format/mkdir/overlay",
		Source: "overlay",
		Options: []string{
			"X-containerd.mkdir.path={{ mount 0 }}/upper:0755",
			"X-containerd.mkdir.path={{ mount 0 }}/work:0755",
			"workdir={{ mount 0 }}/work",
			"upperdir={{ mount 0 }}/upper",
			"lowerdir=" + strings.Join(lowers, ":"),
		},
	})

	return mounts
}
