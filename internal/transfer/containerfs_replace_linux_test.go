//go:build linux

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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

// TestReadPathImportRefusesToReplacePipe covers a file entry landing on a
// pipe, and by extension on a device node, which a container can leave at any
// path it can write.
//
// Opening one for writing would send the archive's bytes into it rather than
// create a file, and for a pipe would block until something opened the read
// end. Removing it instead would leave the container without an interface it
// expects, so the collision is reported and what is there is left alone.
func TestReadPathImportRefusesToReplacePipe(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)

	occupied := filepath.Join(rootfs, replaceEntryName)
	if err := unix.Mkfifo(occupied, 0644); err != nil {
		t.Skipf("cannot create a pipe to extract onto: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- readPath(replacementArchive(t), rootfs, "/", mediaTypeTar, false)
	}()

	select {
	case err := <-done:
		if !errdefs.IsFailedPrecondition(err) {
			t.Fatalf("readPath: err = %v, want FailedPrecondition", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readPath blocked: the pipe was opened for writing")
	}

	fi, err := os.Lstat(occupied)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the pipe was replaced: mode = %v", fi.Mode())
	}
}

// TestReadPathImportLeavesSpecialFileEntriesAlone covers an archive that
// describes a special file, which is what archiving a container's /dev
// produces.
//
// Those entries are not recreated, so the path they name must be left as it
// is. Clearing it to make room for something never written would take away a
// device the container depends on.
func TestReadPathImportLeavesSpecialFileEntriesAlone(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)

	existing := filepath.Join(rootfs, replaceEntryName)
	if err := unix.Mkfifo(existing, 0644); err != nil {
		t.Skipf("cannot create a special file: %v", err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{
			Name:     replaceEntryName,
			Typeflag: tar.TypeFifo,
			Mode:     0644,
		}); err != nil {
			t.Fatal(err)
		}
	})

	if err := readPath(buf, rootfs, "/", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	fi, err := os.Lstat(existing)
	if err != nil {
		t.Fatalf("the special file was removed for an entry that is never created: %v", err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("mode = %v, want a pipe", fi.Mode())
	}
}

// TestPseudoFS checks the filesystems archive operations refuse to touch.
// Kernel-generated ones are named; the ordinary filesystems a container is
// built from must not be, or copying to and from its mounts would break.
func TestPseudoFS(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/proc", "proc"},
		{"/proc/self", "proc"},
		{"/sys", "sysfs"},
		{t.TempDir(), ""},
		{"/", ""},
		// A path that cannot be stat'ed is left to the caller's own
		// operation to report.
		{"/nonexistent-by-design", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if _, err := os.Stat(tc.path); err != nil && tc.want != "" {
				t.Skipf("%s is not present: %v", tc.path, err)
			}
			if got := pseudoFS(tc.path); got != tc.want {
				t.Errorf("pseudoFS(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestWritePathExportRefusesPseudoFS checks that naming a kernel-generated
// filesystem is reported rather than producing a partial archive.
func TestWritePathExportRefusesPseudoFS(t *testing.T) {
	buf := &bytes.Buffer{}
	err := writePath("/", "/proc", buf, mediaTypeTar, false)
	if !errdefs.IsInvalidArgument(err) {
		t.Errorf("writePath from /proc: err = %v, want InvalidArgument", err)
	}
}

// TestReadPathImportRefusesPseudoFS checks the same for the destination.
func TestReadPathImportRefusesPseudoFS(t *testing.T) {
	err := readPath(replacementArchive(t), "/", "/proc", mediaTypeTar, false)
	if !errdefs.IsInvalidArgument(err) {
		t.Errorf("readPath to /proc: err = %v, want InvalidArgument", err)
	}
}

// TestPseudoFSHolder covers the filesystem a path would be created on, which
// is what decides whether extracting there writes a file or reaches into a
// kernel interface. Paths that do not exist yet resolve to their nearest
// existing ancestor, since that is where creating them would land.
func TestPseudoFSHolder(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"existing pseudo-filesystem", "/proc", "proc"},
		{"path below one that does not exist", "/proc/nonexistent/deeper", "proc"},
		{"existing ordinary directory", dir, ""},
		{"path below an ordinary one", filepath.Join(dir, "a", "b"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pseudoFSHolder(tc.path); got != tc.want {
				t.Errorf("pseudoFSHolder(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
