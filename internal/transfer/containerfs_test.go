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

package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
)

// makeRootfs creates a temporary rootfs and a sibling "outside"
// directory used to verify that symlink resolution cannot escape.
// Returns (bundleDir, rootfs, outside).
func makeRootfs(t *testing.T) (string, string, string) {
	t.Helper()
	bundle := t.TempDir()
	rootfs := filepath.Join(bundle, "rootfs")
	outside := filepath.Join(bundle, "outside")
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	return bundle, rootfs, outside
}

// readTar collects all entries from a tar buffer into a map keyed by
// header name, storing each entry's header and the body bytes for
// regular files.
func readTar(t *testing.T, buf *bytes.Buffer) map[string]tarEntry {
	t.Helper()
	entries := map[string]tarEntry{}
	tr := tar.NewReader(buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		var body []byte
		if h.Typeflag == tar.TypeReg {
			body, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read entry %q: %v", h.Name, err)
			}
		}
		entries[h.Name] = tarEntry{header: *h, body: body}
	}
	return entries
}

type tarEntry struct {
	header tar.Header
	body   []byte
}

func writeTar(t *testing.T, build func(tw *tar.Writer)) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	build(tw)
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf
}

// writeLegacyRegularTar writes a raw legacy regular-file typeflag. tar.Writer
// promotes TypeRegA to TypeReg, so the typeflag and checksum must be adjusted
// after writing the archive.
func writeLegacyRegularTar(t *testing.T, name, body string) *bytes.Buffer {
	t.Helper()
	buf := writeTar(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte(body))
	})

	header := buf.Bytes()[:tarBlockSize]
	header[156] = tar.TypeRegA //nolint:staticcheck // Exercise a legacy tar typeflag.
	copy(header[148:156], "        ")
	var checksum int
	for _, b := range header {
		checksum += int(b)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", checksum))
	return buf
}

const tarBlockSize = 512

// TestWritePathExportSymlinkEscapeBlocked verifies that when a tar
// export hits a regular file whose path would resolve outside the
// rootfs (because an intermediate symlink points outside), the open
// fails rather than reading the host file.
func TestWritePathExportSymlinkEscapeBlocked(t *testing.T) {
	_, rootfs, outside := makeRootfs(t)

	// Place a sensitive file outside the rootfs.
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("HOST_SECRET"), 0644); err != nil {
		t.Fatal(err)
	}

	// Inside rootfs: a symlink that points outside.
	if err := os.Symlink(outside, filepath.Join(rootfs, "escape")); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	// Asking to copy /escape/secret. Lstat would have to traverse
	// the symlink "/escape" out of the rootfs to reach "secret".
	err := writePath(rootfs, "/escape/secret", "secret", buf, mediaTypeTar, false)
	if err == nil {
		t.Fatal("expected error when traversing symlink out of rootfs, got nil")
	}
}

// TestWritePathExportPreservesSymlinks verifies that a symlink within
// the rootfs is copied as a symlink (its target string preserved),
// not dereferenced.
func TestWritePathExportPreservesSymlinks(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)

	if err := os.WriteFile(filepath.Join(rootfs, "real"), []byte("OK"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/real", filepath.Join(rootfs, "alias")); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	if err := writePath(rootfs, "/alias", "alias", buf, mediaTypeTar, false); err != nil {
		t.Fatalf("writePath: %v", err)
	}

	entries := readTar(t, buf)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(entries), keys(entries))
	}
	e, ok := entries["alias"]
	if !ok {
		t.Fatalf("missing 'alias' entry: %v", keys(entries))
	}
	if e.header.Typeflag != tar.TypeSymlink {
		t.Fatalf("expected symlink entry, got typeflag %v", e.header.Typeflag)
	}
	if filepath.ToSlash(e.header.Linkname) != "/real" {
		t.Fatalf("expected linkname '/real', got %q", e.header.Linkname)
	}
}

// TestWritePathExportWalkContainsSymlinkToOutside verifies that
// walking a directory whose contents include a symlink pointing
// outside the rootfs does not export the host file's contents — the
// symlink entry is preserved as a symlink instead.
func TestWritePathExportWalkContainsSymlinkToOutside(t *testing.T) {
	_, rootfs, outside := makeRootfs(t)
	if err := os.WriteFile(filepath.Join(outside, "host"), []byte("HOST"), 0644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(rootfs, "dir")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "host"), filepath.Join(dir, "leak")); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	if err := writePath(rootfs, "/dir", "dir", buf, mediaTypeTar, false); err != nil {
		t.Fatalf("writePath: %v", err)
	}

	entries := readTar(t, buf)
	leak, ok := entries["dir/leak"]
	if !ok {
		t.Fatalf("missing 'dir/leak' entry: %v", keys(entries))
	}
	if leak.header.Typeflag != tar.TypeSymlink {
		t.Fatalf("expected symlink entry for leak, got %v", leak.header.Typeflag)
	}
	for name, e := range entries {
		if e.header.Typeflag == tar.TypeReg && bytes.Contains(e.body, []byte("HOST")) {
			t.Fatalf("entry %q leaked host file contents", name)
		}
	}
}

// TestReadPathImportContainsTarEscape verifies that a tar entry whose
// name contains "../" sequences is collapsed into the destination
// directory rather than escaping it. readPath cleans the entry name
// against "/" before passing it to dst, so "../../etc/passwd" lands at
// dst/etc/passwd inside the rootfs.
func TestReadPathImportContainsTarEscape(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "dst"), 0755); err != nil {
		t.Fatal(err)
	}

	// A naive prefix check that doesn't Clean would let this through.
	buf := writeTar(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name:     "../../etc/passwd",
			Mode:     0644,
			Size:     4,
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte("PWND"))
	})

	if err := readPath(buf, rootfs, "/dst", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	// Anything written outside <rootfs>/dst would indicate the
	// traversal succeeded — including the most likely landing spot
	// of <rootfs>/etc/passwd (one level above dst but still inside
	// rootfs).
	if _, err := os.Stat(filepath.Join(rootfs, "etc", "passwd")); err == nil {
		t.Fatal("escape produced a file outside dst (at rootfs/etc/passwd)")
	}
	if _, err := os.Stat(filepath.Join(rootfs, "..", "etc", "passwd")); err == nil {
		t.Fatal("escape produced a file outside rootfs")
	}

	contained := filepath.Join(rootfs, "dst", "etc", "passwd")
	body, err := os.ReadFile(contained)
	if err != nil {
		t.Fatalf("expected entry contained at %s: %v", contained, err)
	}
	if string(body) != "PWND" {
		t.Fatalf("contained entry body: want %q, got %q", "PWND", body)
	}
}

// TestReadPathImportSymlinkRedirectBlocked verifies the classic
// tar-symlink attack: a tar containing a symlink pointing outside
// the rootfs followed by a regular file beneath that symlink does
// not write through the symlink to the host.
func TestReadPathImportSymlinkRedirectBlocked(t *testing.T) {
	_, rootfs, outside := makeRootfs(t)
	target := filepath.Join(outside, "target")

	// Pre-existing host file we want to protect.
	if err := os.WriteFile(target, []byte("ORIGINAL"), 0644); err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		// Symlink "linkdir" -> absolute path outside rootfs.
		_ = tw.WriteHeader(&tar.Header{
			Name:     "linkdir",
			Linkname: outside,
			Typeflag: tar.TypeSymlink,
			Mode:     0777,
		})
		// Then a regular file beneath that symlink.
		_ = tw.WriteHeader(&tar.Header{
			Name:     "linkdir/target",
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     5,
		})
		_, _ = tw.Write([]byte("OWNED"))
	})

	// readPath may return an error or succeed silently; either is
	// acceptable. The invariant is that the host file is NOT
	// modified.
	_ = readPath(buf, rootfs, "/", mediaTypeTar, false)

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("host file was overwritten: got %q", got)
	}
}

// TestReadPathImportPreExistingSymlink verifies that a symlink that
// already exists in the rootfs and points outside cannot be used to
// redirect writes from a subsequent extraction.
func TestReadPathImportPreExistingSymlink(t *testing.T) {
	_, rootfs, outside := makeRootfs(t)
	target := filepath.Join(outside, "target")
	if err := os.WriteFile(target, []byte("ORIGINAL"), 0644); err != nil {
		t.Fatal(err)
	}
	// Pre-existing symlink in the destination directory.
	if err := os.MkdirAll(filepath.Join(rootfs, "dst"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootfs, "dst", "linkdir")); err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name:     "linkdir/target",
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     5,
		})
		_, _ = tw.Write([]byte("OWNED"))
	})

	_ = readPath(buf, rootfs, "/dst", mediaTypeTar, false)

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("host file was overwritten via pre-existing symlink: got %q", got)
	}
}

// TestReadPathImportRoundTrip verifies that a basic tar round-trip
// (regular file, directory, symlink, hardlink) extracts correctly.
func TestReadPathImportRoundTrip(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)

	buf := writeTar(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name:     "d",
			Typeflag: tar.TypeDir,
			Mode:     0755,
		})
		_ = tw.WriteHeader(&tar.Header{
			Name:     "d/file",
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     5,
		})
		_, _ = tw.Write([]byte("hello"))
		_ = tw.WriteHeader(&tar.Header{
			Name:     "d/sym",
			Linkname: "file",
			Typeflag: tar.TypeSymlink,
			Mode:     0777,
		})
		_ = tw.WriteHeader(&tar.Header{
			Name:     "d/hard",
			Linkname: "d/file",
			Typeflag: tar.TypeLink,
			Mode:     0644,
		})
	})

	if err := readPath(buf, rootfs, "/", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(rootfs, "d/file"))
	if err != nil {
		t.Fatalf("read d/file: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("d/file body: %q", body)
	}
	link, err := os.Readlink(filepath.Join(rootfs, "d/sym"))
	if err != nil {
		t.Fatalf("readlink d/sym: %v", err)
	}
	if link != "file" {
		t.Fatalf("d/sym target: %q", link)
	}
	hard, err := os.ReadFile(filepath.Join(rootfs, "d/hard"))
	if err != nil {
		t.Fatalf("read d/hard: %v", err)
	}
	if string(hard) != "hello" {
		t.Fatalf("d/hard body: %q", hard)
	}
}

// TestReadPathImportLegacyRegularFile verifies that the legacy zero typeflag
// extracts as a regular file for both directory and existing-file
// destinations.
func TestReadPathImportLegacyRegularFile(t *testing.T) {
	t.Run("directory destination", func(t *testing.T) {
		_, rootfs, _ := makeRootfs(t)
		if err := os.Mkdir(filepath.Join(rootfs, "dst"), 0755); err != nil {
			t.Fatal(err)
		}

		buf := writeLegacyRegularTar(t, "payload", "legacy")
		if err := readPath(buf, rootfs, "/dst", mediaTypeTar, false); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(rootfs, "dst", "payload"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "legacy" {
			t.Fatalf("payload = %q, want %q", got, "legacy")
		}
	})

	t.Run("file destination", func(t *testing.T) {
		_, rootfs, _ := makeRootfs(t)
		target := filepath.Join(rootfs, "target")
		if err := os.WriteFile(target, []byte("original"), 0644); err != nil {
			t.Fatal(err)
		}

		buf := writeLegacyRegularTar(t, "payload", "legacy")
		if err := readPath(buf, rootfs, "/target", mediaTypeTar, false); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "legacy" {
			t.Fatalf("target = %q, want %q", got, "legacy")
		}
	})
}

// TestRoundTripExportImport writes some files into a rootfs, exports
// them with writePath, then re-imports the tar into a fresh rootfs
// with readPath, and verifies the content matches.
func TestRoundTripExportImport(t *testing.T) {
	_, src, _ := makeRootfs(t)

	files := map[string]string{
		"a/b/c.txt": "alpha",
		"a/b/d.txt": "beta",
		"a/e.txt":   "gamma",
	}
	for name, body := range files {
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	buf := &bytes.Buffer{}
	if err := writePath(src, "/a", "a", buf, mediaTypeTar, false); err != nil {
		t.Fatalf("writePath: %v", err)
	}

	_, dst, _ := makeRootfs(t)
	if err := readPath(buf, dst, "/", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	got := map[string]string{}
	if err := filepath.WalkDir(filepath.Join(dst, "a"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		got[filepath.ToSlash(rel)] = string(body)
		return nil
	}); err != nil {
		t.Fatalf("walk dst: %v", err)
	}

	for name, body := range files {
		if got[name] != body {
			t.Errorf("%s: want %q, got %q", name, body, got[name])
		}
	}
}

func keys(m map[string]tarEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestReadPathImportHardlinkLinknameStaysInDest verifies that a tar
// hardlink whose Linkname contains "../" components cannot reach a
// file outside the caller-specified destination — even when that
// file exists inside the rootfs. The implementation Cleans the
// linkname against "/" so leading "../" sequences collapse, and the
// result is then joined under the destination directory.
func TestReadPathImportHardlinkLinknameStaysInDest(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "dst"), 0755); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(rootfs, "outside")
	if err := os.WriteFile(outsidePath, []byte("X"), 0644); err != nil {
		t.Fatal(err)
	}
	outsideInfo, err := os.Lstat(outsidePath)
	if err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name:     "leak",
			Linkname: "../outside",
			Typeflag: tar.TypeLink,
			Mode:     0644,
		})
	})

	// readPath may succeed (re-rooting the link inside /dst, where
	// "outside" doesn't exist) or fail (because the re-rooted
	// linkname points at a missing file). Either is fine. The
	// invariant is that <rootfs>/outside is not the link target.
	_ = readPath(buf, rootfs, "/dst", mediaTypeTar, false)

	if leakInfo, err := os.Lstat(filepath.Join(rootfs, "dst", "leak")); err == nil {
		if os.SameFile(outsideInfo, leakInfo) {
			t.Fatal("hardlink reached file outside destination")
		}
	}
}

// TestReadPathImportAbsoluteEntryName verifies that a tar entry with
// an absolute name (e.g. "/etc/passwd") is treated as relative to the
// destination, not the host root.
func TestReadPathImportAbsoluteEntryName(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "dst"), 0755); err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name:     "/etc/passwd",
			Mode:     0644,
			Size:     2,
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte("xx"))
	})

	if err := readPath(buf, rootfs, "/dst", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	// File should land at <rootfs>/dst/etc/passwd, NOT /etc/passwd
	// and NOT <rootfs>/etc/passwd.
	if _, err := os.Stat(filepath.Join(rootfs, "dst", "etc", "passwd")); err != nil {
		t.Fatalf("expected file under dst/etc/passwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "etc", "passwd")); err == nil {
		t.Fatal("absolute entry name escaped to <rootfs>/etc/passwd")
	}
}

// TestReadPathImportNestedDotDotInName verifies that an entry whose
// path uses ../ components mid-name (e.g. "subdir/../../etc/passwd")
// is collapsed by Clean and lands inside the destination, never at
// the rootfs root. This is a subtler case than a leading "../"
// because the path appears benign until Clean simplifies it.
func TestReadPathImportNestedDotDotInName(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "dst"), 0755); err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name:     "subdir/../../etc/passwd",
			Mode:     0644,
			Size:     2,
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte("xx"))
	})

	if err := readPath(buf, rootfs, "/dst", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	// Clean("/" + "subdir/../../etc/passwd") == "/etc/passwd", so the
	// entry must land at <rootfs>/dst/etc/passwd.
	if _, err := os.Stat(filepath.Join(rootfs, "dst", "etc", "passwd")); err != nil {
		t.Fatalf("expected dst/etc/passwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "etc", "passwd")); err == nil {
		t.Fatal("nested ../ entry leaked outside the destination")
	}
}

// TestReadPathImportSymlinkAbsoluteTargetReroutedInRoot verifies that
// a symlink with an absolute target (e.g. "/") is created literally
// but, when later traversed via os.Root, is re-rooted at the rootfs
// rather than the host root. We then attempt to write through it and
// confirm the host file system is untouched.
func TestReadPathImportSymlinkAbsoluteTargetReroutedInRoot(t *testing.T) {
	_, rootfs, outside := makeRootfs(t)
	hostFile := filepath.Join(outside, "host")
	if err := os.WriteFile(hostFile, []byte("ORIGINAL"), 0644); err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		// Create a directory in the rootfs that mirrors the host
		// file's basename so that, if the symlink were resolved
		// against the host, "/host" would name the host file.
		_ = tw.WriteHeader(&tar.Header{
			Name:     "host",
			Typeflag: tar.TypeDir,
			Mode:     0755,
		})
		// A symlink with an absolute target. Created verbatim by
		// root.Symlink — but any later read/write through it goes
		// via os.Root, which interprets "/" as the rootfs.
		_ = tw.WriteHeader(&tar.Header{
			Name:     "abs",
			Linkname: "/host",
			Typeflag: tar.TypeSymlink,
			Mode:     0777,
		})
		// Write through the symlink. Must land at <rootfs>/host/payload,
		// not at <outside>/payload.
		_ = tw.WriteHeader(&tar.Header{
			Name:     "abs/payload",
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     5,
		})
		_, _ = tw.Write([]byte("OWNED"))
	})

	_ = readPath(buf, rootfs, "/", mediaTypeTar, false)

	got, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("host file overwritten via absolute symlink: got %q", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); err == nil {
		t.Fatal("payload written outside rootfs via absolute symlink")
	}
}

// TestWritePathExportRelativeDotDotPath verifies that a path argument
// containing leading "../" components cannot escape the rootfs — it
// is collapsed by Clean before being interpreted.
func TestWritePathExportRelativeDotDotPath(t *testing.T) {
	_, rootfs, outside := makeRootfs(t)
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("HOST"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "inside.txt"), []byte("OK"), 0644); err != nil {
		t.Fatal(err)
	}

	// "../outside/secret" cleans to "outside/secret" (relative),
	// which doesn't exist inside the rootfs.
	buf := &bytes.Buffer{}
	err := writePath(rootfs, "../outside/secret", "secret", buf, mediaTypeTar, false)
	if err == nil {
		t.Fatal("expected error for path escaping rootfs, got nil")
	}
	if buf.Len() != 0 {
		entries := readTar(t, buf)
		for _, e := range entries {
			if bytes.Contains(e.body, []byte("HOST")) {
				t.Fatalf("export contained host secret bytes")
			}
		}
	}
}

// TestWritePathExportNoWalk verifies that noWalk=true on a directory
// produces only the directory entry, without descending.
func TestWritePathExportNoWalk(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "d"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "d", "child"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	if err := writePath(rootfs, "/d", "d", buf, mediaTypeTar, true); err != nil {
		t.Fatalf("writePath: %v", err)
	}

	entries := readTar(t, buf)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry with noWalk, got %d: %v", len(entries), keys(entries))
	}
	if _, ok := entries["d"]; !ok {
		t.Fatalf("expected single 'd' entry, got %v", keys(entries))
	}
}

// TestWritePathExportRootDoesNotLeakBundleName verifies that
// exporting "/" produces tar entries whose names do NOT include the
// host-side rootfs directory's basename. The bundle path is an
// implementation detail of the host and must not appear on the wire.
func TestWritePathExportRootDoesNotLeakBundleName(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "bin", "sh"), []byte("X"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc.conf"), []byte("Y"), 0644); err != nil {
		t.Fatal(err)
	}

	leaked := filepath.Base(rootfs) // e.g. "rootfs"

	buf := &bytes.Buffer{}
	if err := writePath(rootfs, "/", ".", buf, mediaTypeTar, false); err != nil {
		t.Fatalf("writePath: %v", err)
	}

	entries := readTar(t, buf)
	for name := range entries {
		if name == leaked || strings.HasPrefix(name, leaked+"/") {
			t.Errorf("tar entry %q leaks host bundle directory name %q", name, leaked)
		}
	}
	// The actual files should appear with their natural names,
	// without any host-side prefix.
	if _, ok := entries["bin/sh"]; !ok {
		t.Errorf("expected 'bin/sh' entry, got %v", keys(entries))
	}
	if _, ok := entries["etc.conf"]; !ok {
		t.Errorf("expected 'etc.conf' entry, got %v", keys(entries))
	}
}

// TestRoundTripExportRootImport verifies that exporting "/" and
// re-importing into a fresh rootfs reproduces the file tree at the
// destination root, with no extra nesting from a leaked bundle name.
func TestRoundTripExportRootImport(t *testing.T) {
	_, src, _ := makeRootfs(t)
	files := map[string]string{
		"bin/sh":   "shell",
		"etc/conf": "config",
		"top.txt":  "top",
	}
	for name, body := range files {
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	buf := &bytes.Buffer{}
	if err := writePath(src, "/", ".", buf, mediaTypeTar, false); err != nil {
		t.Fatalf("writePath: %v", err)
	}

	_, dst, _ := makeRootfs(t)
	if err := readPath(buf, dst, "/", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s: want %q, got %q", name, want, got)
		}
	}
	// And confirm nothing was nested under the source's bundle name.
	leaked := filepath.Base(src)
	if _, err := os.Stat(filepath.Join(dst, leaked)); err == nil {
		t.Fatalf("unexpected directory %q at destination root — bundle name leaked", leaked)
	}
}

// TestRootRel exercises the path-normalization helper used to convert
// caller-supplied container-view paths to *os.Root-relative paths.
func TestRootRel(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "."},
		{"/", "."},
		{".", "."},
		{"foo", "foo"},
		{"/foo", "foo"},
		{"/foo/bar", "foo/bar"},
		{"foo/bar", "foo/bar"},
		{"/foo/../bar", "bar"},
		{"../foo", "foo"},    // Clean("/" + "../foo") => "/foo"
		{"../../foo", "foo"}, // same — leading .. collapsed against /
		{"/foo/./bar", "foo/bar"},
		{"/foo//bar", "foo/bar"},
	}
	for _, tc := range cases {
		got := rootRel(tc.in)
		if got != tc.want {
			t.Errorf("rootRel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWritePathExportRootDotfilesPreserved verifies that files whose
// names begin with "." (dotfiles) at the rootfs root are exported with
// their full name intact when src is "/". The double-TrimPrefix in the
// walk loop incorrectly strips the leading "." because relPath is also
// "." — exposing this regression.
func TestWritePathExportRootDotfilesPreserved(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)

	if err := os.WriteFile(filepath.Join(rootfs, ".bashrc"), []byte("rc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "plain"), []byte("plain"), 0644); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	if err := writePath(rootfs, "/", ".", buf, mediaTypeTar, false); err != nil {
		t.Fatalf("writePath: %v", err)
	}

	entries := readTar(t, buf)
	if _, ok := entries[".bashrc"]; !ok {
		t.Errorf("dotfile '.bashrc' missing from tar; got entries: %v", keys(entries))
	}
	if _, ok := entries["bashrc"]; ok {
		t.Errorf("dotfile was renamed to 'bashrc' (leading dot stripped by TrimPrefix bug)")
	}
}

// writeBundleSpec writes a config.json declaring bind mounts in the order
// provided.
func writeBundleSpec(t *testing.T, bundle string, binds ...specMount) {
	t.Helper()
	for i := range binds {
		binds[i].Type = "bind"
	}
	writeBundleSpecOpts(t, bundle, false, binds)
}

// TestResolveMountRootNoSpec resolves to the rootfs when the bundle carries no
// config.json, so a bundle without mount information behaves as before.
func TestResolveMountRootNoSpec(t *testing.T) {
	bundle, rootfs, _ := makeRootfs(t)

	root, rel, _, err := resolveMountRoot(bundle, "/etc/hosts")
	if err != nil {
		t.Fatal(err)
	}
	if root != rootfs {
		t.Fatalf("root = %q, want %q", root, rootfs)
	}
	if rel != "/etc/hosts" {
		t.Fatalf("rel = %q, want %q", rel, "/etc/hosts")
	}
}

// TestResolveMountRootUsesLastMatchingDestination pins OCI mount ordering: a
// later parent mount hides an earlier child just as a later child overlays an
// earlier parent.
func TestResolveMountRootUsesLastMatchingDestination(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mounts       []specMount
		wantRoot     string
		wantRel      string
		wantReadonly bool
	}{
		{
			name: "parent before child",
			mounts: []specMount{
				{Destination: "/data", Type: "bind", Source: "/mnt/outer", Options: []string{"ro"}},
				{Destination: "/data/inner", Type: "bind", Source: "/mnt/inner", Options: []string{"rw"}},
			},
			wantRoot: "/mnt/inner",
			wantRel:  "/file",
		},
		{
			name: "child before parent",
			mounts: []specMount{
				{Destination: "/data/inner", Type: "bind", Source: "/mnt/inner", Options: []string{"rw"}},
				{Destination: "/data", Type: "bind", Source: "/mnt/outer", Options: []string{"ro"}},
			},
			wantRoot:     "/mnt/outer",
			wantRel:      "/inner/file",
			wantReadonly: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle, _, _ := makeRootfs(t)
			writeBundleSpecOpts(t, bundle, false, tc.mounts)

			root, rel, readonly, err := resolveMountRoot(bundle, "/data/inner/file")
			if err != nil {
				t.Fatal(err)
			}
			if root != tc.wantRoot || rel != tc.wantRel || readonly != tc.wantReadonly {
				t.Errorf("resolved to (%q, %q, readonly=%v), want (%q, %q, readonly=%v)",
					root, rel, readonly, tc.wantRoot, tc.wantRel, tc.wantReadonly)
			}
		})
	}
}

func TestResolveMountRootMatchesPathBoundary(t *testing.T) {
	bundle, rootfs, _ := makeRootfs(t)
	writeBundleSpec(t, bundle, specMount{Destination: "/data", Source: "/mnt/data"})

	for _, tc := range []struct {
		path     string
		wantRoot string
		wantRel  string
	}{
		{"/data/file", "/mnt/data", "/file"},
		{"/data", "/mnt/data", "."},
		{"/elsewhere/file", rootfs, "/elsewhere/file"},
		// A sibling whose name merely shares the prefix is not inside the mount.
		{"/database", rootfs, "/database"},
	} {
		root, rel, _, err := resolveMountRoot(bundle, tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if root != tc.wantRoot || rel != tc.wantRel {
			t.Errorf("%s -> (%q, %q), want (%q, %q)", tc.path, root, rel, tc.wantRoot, tc.wantRel)
		}
	}
}

// TestReadPathImportLandsInBindSource is the observable effect on the import
// side: extracting to a bind-mounted destination must produce the file in the
// mount's source directory, where the container reads it through the mount —
// not in the shadowed rootfs entry underneath.
func TestReadPathImportLandsInBindSource(t *testing.T) {
	bundle, rootfs, _ := makeRootfs(t)
	source := filepath.Join(bundle, "bind-source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	// The shadowed directory exists in the rootfs, as it does for a real
	// container: the runtime creates the mount point before mounting over it.
	if err := os.MkdirAll(filepath.Join(rootfs, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle, specMount{Destination: "/data", Source: source})

	root, rel, _, err := resolveMountRoot(bundle, "/data")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("through the mount\n")
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "payload.txt",
		Mode:     0644,
		Size:     int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := readPath(&buf, root, rel, mediaTypeTar, false); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(source, "payload.txt"))
	if err != nil {
		t.Fatalf("file missing from the bind source: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("content = %q, want %q", got, body)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "data", "payload.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("file was written to the shadowed rootfs entry, where the container cannot see it")
	}
}

// TestWritePathExportReadsBindSource is the same effect on the export side: an
// archive of a bind-mounted path must carry the mounted content, not whatever
// the shadowed rootfs entry holds.
func TestWritePathExportReadsBindSource(t *testing.T) {
	bundle, rootfs, _ := makeRootfs(t)
	source := filepath.Join(bundle, "bind-source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload.txt"), []byte("mounted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Same name under the shadowed rootfs entry, with different content: if
	// resolution is wrong the export silently returns this instead.
	if err := os.MkdirAll(filepath.Join(rootfs, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "data", "payload.txt"), []byte("shadowed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle, specMount{Destination: "/data", Source: source})

	root, rel, _, err := resolveMountRoot(bundle, "/data/payload.txt")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writePath(root, rel, "payload.txt", &buf, mediaTypeTar, false); err != nil {
		t.Fatal(err)
	}

	entries := readTar(t, &buf)
	e, ok := entries["payload.txt"]
	if !ok {
		t.Fatalf("payload.txt missing from archive, got %v", entries)
	}
	if string(e.body) != "mounted\n" {
		t.Fatalf("archived %q, want the mounted content", e.body)
	}
}

// TestResolveMountRootSingleFileMount pins resolution for bind mounts whose
// source is a file: the root is the file's parent directory (an *os.Root
// cannot anchor at a file), and a relative source is interpreted against the
// bundle directory, as the runtime does for bundle extra files.
func TestResolveMountRootSingleFileMount(t *testing.T) {
	bundle, _, _ := makeRootfs(t)
	if err := os.WriteFile(filepath.Join(bundle, "resolv.conf"), []byte("nameserver 10.0.0.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(bundle, "extra")
	if err := os.MkdirAll(extra, 0755); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(extra, "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle,
		specMount{Destination: "/etc/resolv.conf", Source: "resolv.conf"}, // relative to the bundle
		specMount{Destination: "/etc/hosts", Source: hosts},               // absolute
	)

	for _, tc := range []struct {
		path     string
		wantRoot string
		wantRel  string
	}{
		{"/etc/resolv.conf", bundle, "resolv.conf"},
		{"/etc/hosts", extra, "hosts"},
		// A path below a file mount cannot exist; the residual rel makes
		// the caller's stat fail with ENOTDIR rather than silently
		// resolving elsewhere.
		{"/etc/hosts/sub", extra, "hosts/sub"},
	} {
		root, rel, _, err := resolveMountRoot(bundle, tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if root != tc.wantRoot || rel != tc.wantRel {
			t.Errorf("%s -> (%q, %q), want (%q, %q)", tc.path, root, rel, tc.wantRoot, tc.wantRel)
		}
	}
}

// TestWritePathExportSingleFileBindMount exports a file-mount destination:
// the archive must carry the mounted bytes under the container-view name,
// even though the source file's own basename differs.
func TestWritePathExportSingleFileBindMount(t *testing.T) {
	bundle, rootfs, _ := makeRootfs(t)
	source := filepath.Join(bundle, "resolv-generated.conf")
	if err := os.WriteFile(source, []byte("nameserver 10.0.0.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Shadowed rootfs entry with different content: the image may ship its
	// own resolv.conf under the mount point.
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "resolv.conf"), []byte("shadowed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle, specMount{Destination: "/etc/resolv.conf", Source: source})

	root, rel, _, err := resolveMountRoot(bundle, "/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writePath(root, rel, "resolv.conf", &buf, mediaTypeTar, false); err != nil {
		t.Fatal(err)
	}

	entries := readTar(t, &buf)
	e, ok := entries["resolv.conf"]
	if !ok {
		t.Fatalf("resolv.conf missing from archive, got %v", keys(entries))
	}
	if string(e.body) != "nameserver 10.0.0.1\n" {
		t.Fatalf("archived %q, want the mounted content", e.body)
	}
}

// TestReadPathImportOverSingleFileBindMount imports onto a file-mount
// destination: the mount source's bytes are replaced in place — same inode,
// so the container's mount keeps seeing the file — and the shadowed rootfs
// entry stays untouched.
func TestReadPathImportOverSingleFileBindMount(t *testing.T) {
	bundle, rootfs, _ := makeRootfs(t)
	source := filepath.Join(bundle, "resolv.conf")
	if err := os.WriteFile(source, []byte("nameserver 10.0.0.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	stagingLookalike := source + ".transfer-tmp"
	if err := os.WriteFile(stagingLookalike, []byte("keep\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "resolv.conf"), []byte("shadowed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle, specMount{Destination: "/etc/resolv.conf", Source: "resolv.conf"})

	before, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}

	root, rel, _, err := resolveMountRoot(bundle, "/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		body := []byte("nameserver 10.0.0.2\n")
		_ = tw.WriteHeader(&tar.Header{
			Name:     "resolv.conf",
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     int64(len(body)),
		})
		_, _ = tw.Write(body)
	})

	if err := readPath(buf, root, rel, mediaTypeTar, false); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nameserver 10.0.0.2\n" {
		t.Fatalf("source content = %q, want the imported bytes", got)
	}
	after, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("source was replaced by a new inode; the container's mount would keep the stale file")
	}
	shadow, err := os.ReadFile(filepath.Join(rootfs, "etc", "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(shadow) != "shadowed\n" {
		t.Fatalf("shadowed rootfs entry was modified: %q", shadow)
	}
	lookalike, err := os.ReadFile(stagingLookalike)
	if err != nil {
		t.Fatal(err)
	}
	if string(lookalike) != "keep\n" {
		t.Fatalf("staging lookalike was modified: %q", lookalike)
	}
}

// TestReadPathImportDirectoryOverFileFails rejects extracting a directory
// archive over a file-mount destination, mirroring "cannot copy a directory
// to a file" semantics.
func TestReadPathImportDirectoryOverFileFails(t *testing.T) {
	bundle, _, _ := makeRootfs(t)
	source := filepath.Join(bundle, "resolv.conf")
	if err := os.WriteFile(source, []byte("nameserver 10.0.0.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle, specMount{Destination: "/etc/resolv.conf", Source: source})

	root, rel, _, err := resolveMountRoot(bundle, "/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{
			Name:     "d",
			Typeflag: tar.TypeDir,
			Mode:     0755,
		})
	})

	if err := readPath(buf, root, rel, mediaTypeTar, false); err == nil {
		t.Fatal("expected error extracting a directory over a file destination")
	}
	got, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nameserver 10.0.0.1\n" {
		t.Fatalf("source was modified by a failed import: %q", got)
	}
}

// TestReadPathImportMultipleEntriesOverFileLeavesTargetUntouched rejects a
// multi-entry archive at a file destination without touching the file.
func TestReadPathImportMultipleEntriesOverFileLeavesTargetUntouched(t *testing.T) {
	bundle, _, _ := makeRootfs(t)
	source := filepath.Join(bundle, "resolv.conf")
	if err := os.WriteFile(source, []byte("nameserver 10.0.0.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle, specMount{Destination: "/etc/resolv.conf", Source: source})

	root, rel, _, err := resolveMountRoot(bundle, "/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		for _, name := range []string{"first", "second"} {
			body := []byte("OWNED " + name + "\n")
			_ = tw.WriteHeader(&tar.Header{
				Name:     name,
				Typeflag: tar.TypeReg,
				Mode:     0644,
				Size:     int64(len(body)),
			})
			_, _ = tw.Write(body)
		}
	})

	if err := readPath(buf, root, rel, mediaTypeTar, false); err == nil {
		t.Fatal("expected error extracting multiple entries over a file destination")
	}
	got, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nameserver 10.0.0.1\n" {
		t.Fatalf("target was modified by a failed import: %q", got)
	}
	entries, err := os.ReadDir(filepath.Dir(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".transfer-") {
			t.Fatalf("staging file left behind after a failed import: %s", entry.Name())
		}
	}
}

// TestReadPathImportEmptyArchiveOverFileFails rejects an empty archive at a
// file destination instead of succeeding without replacing anything.
func TestReadPathImportEmptyArchiveOverFileFails(t *testing.T) {
	bundle, _, _ := makeRootfs(t)
	source := filepath.Join(bundle, "resolv.conf")
	if err := os.WriteFile(source, []byte("nameserver 10.0.0.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle, specMount{Destination: "/etc/resolv.conf", Source: source})

	root, rel, _, err := resolveMountRoot(bundle, "/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {})

	if err := readPath(buf, root, rel, mediaTypeTar, false); err == nil {
		t.Fatal("expected error extracting an empty archive over a file destination")
	}
	got, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nameserver 10.0.0.1\n" {
		t.Fatalf("source was modified by a failed import: %q", got)
	}
}

// TestReadPathImportOverSymlinkDestinationFails verifies that only an
// existing regular file activates the single-file import path. A symlink is
// not treated as a file destination, even when it points to a regular file.
func TestReadPathImportOverSymlinkDestinationFails(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)
	target := filepath.Join(rootfs, "target")
	if err := os.WriteFile(target, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(rootfs, "destination")); err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		body := []byte("replacement")
		_ = tw.WriteHeader(&tar.Header{
			Name:     "payload",
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     int64(len(body)),
		})
		_, _ = tw.Write(body)
	})

	if err := readPath(buf, rootfs, "/destination", mediaTypeTar, false); err == nil {
		t.Fatal("expected error extracting over a symlink destination")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("symlink target was modified: %q", got)
	}
}

// TestWritePathExportDirMountExactKeepsName pins the naming contract when the
// requested path is exactly a directory mount's destination: the walk anchors
// at the mount source, but the archive's top-level name is the destination's
// basename as the container sees it — Transfer derives it from the container
// path, not from the resolved source.
func TestWritePathExportDirMountExactKeepsName(t *testing.T) {
	bundle, _, _ := makeRootfs(t)
	source := filepath.Join(bundle, "bind-source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBundleSpec(t, bundle, specMount{Destination: "/data", Source: source})

	root, rel, _, err := resolveMountRoot(bundle, "/data")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writePath(root, rel, "data", &buf, mediaTypeTar, false); err != nil {
		t.Fatal(err)
	}

	entries := readTar(t, &buf)
	if _, ok := entries["data/file"]; !ok {
		t.Fatalf("expected 'data/file' entry, got %v", keys(entries))
	}
	if _, ok := entries[filepath.Base(source)+"/file"]; ok {
		t.Fatal("archive leaked the mount source's basename instead of the container-view name")
	}
}

type specMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

func writeBundleSpecOpts(t *testing.T, bundle string, rootReadonly bool, mounts []specMount) {
	t.Helper()
	spec := struct {
		Root struct {
			Readonly bool `json:"readonly"`
		} `json:"root"`
		Mounts []specMount `json:"mounts"`
	}{Mounts: mounts}
	spec.Root.Readonly = rootReadonly
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveMountRootReadOnly pins the readonly flag: the last ro/rw option
// wins, and a path no mount covers falls back to the spec's root flag.
func TestResolveMountRootReadOnly(t *testing.T) {
	bundle, _, _ := makeRootfs(t)
	writeBundleSpecOpts(t, bundle, true, []specMount{
		{Destination: "/ro", Type: "bind", Source: "/mnt/ro", Options: []string{"rbind", "ro"}},
		{Destination: "/rw", Type: "bind", Source: "/mnt/rw", Options: []string{"rbind"}},
		{Destination: "/ro-then-rw", Type: "bind", Source: "/mnt/a", Options: []string{"rbind", "ro", "rw"}},
		{Destination: "/rw-then-ro", Type: "bind", Source: "/mnt/b", Options: []string{"rbind", "rw", "ro"}},
	})

	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/ro/file", true},
		{"/rw/file", false},
		{"/ro-then-rw/file", false},
		{"/rw-then-ro/file", true},
		// No mount covers the path: the read-only root decides.
		{"/etc/hosts", true},
	} {
		_, _, readonly, err := resolveMountRoot(bundle, tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if readonly != tc.want {
			t.Errorf("%s: readonly = %v, want %v", tc.path, readonly, tc.want)
		}
	}
}

func TestResolveMountRootDuplicateDestinationUsesLast(t *testing.T) {
	bundle, _, _ := makeRootfs(t)
	writeBundleSpecOpts(t, bundle, false, []specMount{
		{Destination: "/data", Type: "bind", Source: "/mnt/first", Options: []string{"ro"}},
		{Destination: "/data", Type: "bind", Source: "/mnt/second", Options: []string{"rw"}},
	})

	root, rel, readonly, err := resolveMountRoot(bundle, "/data/file")
	if err != nil {
		t.Fatal(err)
	}
	if root != "/mnt/second" || rel != "/file" || readonly {
		t.Fatalf("resolved to (%q, %q, readonly=%v), want (%q, %q, readonly=false)",
			root, rel, readonly, "/mnt/second", "/file")
	}
}

// TestTransferImportToReadOnlyPathRejected verifies copy-to into a read-only
// mount or rootfs fails with ErrPermissionDenied, the backing bytes intact.
func TestTransferImportToReadOnlyPathRejected(t *testing.T) {
	newBundle := func(t *testing.T) (bundleParent, bundle string) {
		t.Helper()
		bundleParent = t.TempDir()
		bundle = filepath.Join(bundleParent, "c1")
		if err := os.MkdirAll(filepath.Join(bundle, "rootfs"), 0755); err != nil {
			t.Fatal(err)
		}
		return bundleParent, bundle
	}

	// A ReadStream with no backing stream asserts the rejection precedes any read.
	transferTo := func(t *testing.T, bundleParent, containerPath string) error {
		t.Helper()
		return NewContainerFSTransferrer(bundleParent).Transfer(context.Background(),
			&ReadStream{MediaType: mediaTypeTar},
			&ContainerPath{ContainerID: "c1", Path: containerPath})
	}

	t.Run("read-only bind mount", func(t *testing.T) {
		bundleParent, bundle := newBundle(t)
		source := filepath.Join(bundle, "bind-source")
		if err := os.MkdirAll(source, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "keep"), []byte("ORIGINAL"), 0644); err != nil {
			t.Fatal(err)
		}
		writeBundleSpecOpts(t, bundle, false, []specMount{
			{Destination: "/data", Type: "bind", Source: source, Options: []string{"rbind", "ro"}},
		})

		err := transferTo(t, bundleParent, "/data/keep")
		if !errdefs.IsPermissionDenied(err) {
			t.Fatalf("expected permission-denied error, got %v", err)
		}
		got, err := os.ReadFile(filepath.Join(source, "keep"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "ORIGINAL" {
			t.Fatalf("read-only mount source was modified: %q", got)
		}
	})

	t.Run("read-only rootfs", func(t *testing.T) {
		bundleParent, bundle := newBundle(t)
		writeBundleSpecOpts(t, bundle, true, nil)

		if err := transferTo(t, bundleParent, "/etc/hosts"); !errdefs.IsPermissionDenied(err) {
			t.Fatalf("expected permission-denied error, got %v", err)
		}
	})
}
