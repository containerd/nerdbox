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
)

// extractedEntry is what the archive used by the replacement tests below
// carries: one regular file, always at the same name.
const (
	replaceEntryName    = "entry"
	replaceEntryContent = "extracted\n"
)

func replacementArchive(t *testing.T) *bytes.Buffer {
	t.Helper()
	return writeTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{
			Name:     replaceEntryName,
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     int64(len(replaceEntryContent)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(replaceEntryContent)); err != nil {
			t.Fatal(err)
		}
	})
}

// TestReadPathImportReplacesExistingEntries covers the ordinary collisions:
// whatever occupies the entry's own name gives way to it.
func TestReadPathImportReplacesExistingEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		occupy  func(t *testing.T, path string)
		outside string
	}{
		{
			name: "symlink",
			occupy: func(t *testing.T, path string) {
				if err := os.Symlink("/somewhere-else", path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty directory",
			occupy: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "regular file",
			occupy: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("previous\n"), 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rootfs, _ := makeRootfs(t)

			occupied := filepath.Join(rootfs, replaceEntryName)
			tc.occupy(t, occupied)

			if err := readPath(replacementArchive(t), rootfs, "/", mediaTypeTar, false); err != nil {
				t.Fatalf("readPath: %v", err)
			}

			fi, err := os.Lstat(occupied)
			if err != nil {
				t.Fatal(err)
			}
			if !fi.Mode().IsRegular() {
				t.Fatalf("mode = %v, want a regular file", fi.Mode())
			}
			got, err := os.ReadFile(occupied)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != replaceEntryContent {
				t.Errorf("content = %q, want %q", got, replaceEntryContent)
			}
		})
	}
}

// TestReadPathImportMergesDirectories pins the one case that is not
// replaced: a directory entry landing on an existing directory keeps what is
// already there, so extracting into a populated tree adds to it.
func TestReadPathImportMergesDirectories(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)

	existing := filepath.Join(rootfs, "dir")
	if err := os.MkdirAll(existing, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "kept"), []byte("kept\n"), 0644); err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{
			Name:     "dir/",
			Typeflag: tar.TypeDir,
			Mode:     0755,
		}); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     "dir/added",
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     int64(len(replaceEntryContent)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(replaceEntryContent)); err != nil {
			t.Fatal(err)
		}
	})

	if err := readPath(buf, rootfs, "/", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	if _, err := os.Stat(filepath.Join(existing, "kept")); err != nil {
		t.Errorf("the existing directory was replaced rather than merged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(existing, "added")); err != nil {
		t.Errorf("the archived entry is missing: %v", err)
	}
}

// TestReadPathImportFollowsSymlinkedParent checks that replacement is
// confined to the entry's own name. A symlinked directory earlier in the
// path is still followed, which is how a path resolves for the container.
func TestReadPathImportFollowsSymlinkedParent(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)

	targetDir := filepath.Join(rootfs, "real")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(rootfs, "link")); err != nil {
		t.Fatal(err)
	}

	buf := writeTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{
			Name:     "link/" + replaceEntryName,
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     int64(len(replaceEntryContent)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(replaceEntryContent)); err != nil {
			t.Fatal(err)
		}
	})

	if err := readPath(buf, rootfs, "/", mediaTypeTar, false); err != nil {
		t.Fatalf("readPath: %v", err)
	}

	// The entry landed through the symlink, and the symlink survived.
	if got, err := os.ReadFile(filepath.Join(targetDir, replaceEntryName)); err != nil {
		t.Errorf("the entry did not land through the symlinked parent: %v", err)
	} else if string(got) != replaceEntryContent {
		t.Errorf("content = %q, want %q", got, replaceEntryContent)
	}
	fi, err := os.Lstat(filepath.Join(rootfs, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlinked parent was replaced")
	}
}

// TestReadPathImportReportsPopulatedDirectoryConflict pins the limit of that
// replacement. An entry whose name is taken by a directory with contents is a
// conflict the caller is told about, rather than a tree the extraction quietly
// deletes to make room.
func TestReadPathImportReportsPopulatedDirectoryConflict(t *testing.T) {
	_, rootfs, _ := makeRootfs(t)

	occupied := filepath.Join(rootfs, replaceEntryName)
	if err := os.MkdirAll(filepath.Join(occupied, "child"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "child", "data"), []byte("keep\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := readPath(replacementArchive(t), rootfs, "/", mediaTypeTar, false); err == nil {
		t.Fatal("readPath replaced a populated directory instead of reporting the conflict")
	}

	if got, err := os.ReadFile(filepath.Join(occupied, "child", "data")); err != nil {
		t.Errorf("the directory contents were deleted: %v", err)
	} else if string(got) != "keep\n" {
		t.Errorf("content = %q, want %q", got, "keep\n")
	}
}
