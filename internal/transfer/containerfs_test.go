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
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	err := writePath(rootfs, "/escape/secret", buf, mediaTypeTar, false)
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
	if err := writePath(rootfs, "/alias", buf, mediaTypeTar, false); err != nil {
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
	if err := writePath(rootfs, "/dir", buf, mediaTypeTar, false); err != nil {
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
	if err := writePath(src, "/a", buf, mediaTypeTar, false); err != nil {
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
	err := writePath(rootfs, "../outside/secret", buf, mediaTypeTar, false)
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
	if err := writePath(rootfs, "/d", buf, mediaTypeTar, true); err != nil {
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
	if err := writePath(rootfs, "/", buf, mediaTypeTar, false); err != nil {
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
	if err := writePath(src, "/", buf, mediaTypeTar, false); err != nil {
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
	if err := writePath(rootfs, "/", buf, mediaTypeTar, false); err != nil {
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
