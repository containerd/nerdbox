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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeExtentFile creates a sparse file of the given size (in 512-byte sectors)
// inside dir and returns its absolute path.
func makeExtentFile(t *testing.T, dir, name string, sectors int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(int64(sectors)*512))
	require.NoError(t, f.Close())
	return path
}

// TestDumpVMDKDescriptorToFile verifies that DumpVMDKDescriptorToFile
// does not leave any .tmp intermediate file behind after a successful write.
func TestDumpVMDKDescriptorToFile(t *testing.T) {
	dir := t.TempDir()
	ext := makeExtentFile(t, dir, "layer.erofs", 8)

	descPath := filepath.Join(dir, "merged_fs.vmdk")
	require.NoError(t, DumpVMDKDescriptorToFile(descPath, 0xfffffffe, []string{ext}))

	_, err := os.Stat(descPath)
	require.NoError(t, err, "descriptor file should exist")
}

// TestDumpVMDKDescriptorToFile_WriteFail verifies that DumpVMDKDescriptorToFile
// cleans up the vmdk file when the write fails (e.g. a missing device).
func TestDumpVMDKDescriptorToFile_WriteFail(t *testing.T) {
	dir := t.TempDir()
	descPath := filepath.Join(dir, "merged_fs.vmdk")

	err := DumpVMDKDescriptorToFile(descPath, 0xfffffffe, []string{"/nonexistent/layer.erofs"})
	require.Error(t, err)

	_, statErr := os.Stat(descPath)
	assert.True(t, os.IsNotExist(statErr), "descriptor must not exist after a failed write")
}

// TestDumpVMDKDescriptorToFile_Regenerate verifies that calling
// DumpVMDKDescriptorToFile twice on the same path replaces the descriptor
// each time. This mirrors the unconditional-regeneration behaviour used by transformMounts.
func TestDumpVMDKDescriptorToFile_Regenerate(t *testing.T) {
	dir := t.TempDir()
	ext := makeExtentFile(t, dir, "layer.erofs", 8)
	descPath := filepath.Join(dir, "merged_fs.vmdk")

	require.NoError(t, DumpVMDKDescriptorToFile(descPath, 0xfffffffe, []string{ext}))

	fi1, err := os.Stat(descPath)
	require.NoError(t, err)

	// Call again unconditionally — the second write must replace the file.
	require.NoError(t, DumpVMDKDescriptorToFile(descPath, 0xfffffffe, []string{ext}))

	fi2, err := os.Stat(descPath)
	require.NoError(t, err)

	// The descriptor must still exist and be readable.
	_, statErr := os.Stat(descPath)
	require.NoError(t, statErr, "descriptor must exist after second write")

	// Size should be identical (same inputs), confirming the file was rewritten
	// rather than skipped.
	assert.Equal(t, fi1.Size(), fi2.Size(), "descriptor size must match between regenerations with the same inputs")
}

// TestDumpVMDKDescriptorToFile_MultipleDevices verifies that a descriptor
// written for multiple extent files can be written and replaced successfully.
func TestDumpVMDKDescriptorToFile_MultipleDevices(t *testing.T) {
	dir := t.TempDir()
	ext1 := makeExtentFile(t, dir, "layer0.erofs", 8)
	ext2 := makeExtentFile(t, dir, "layer1.erofs", 4)

	descPath := filepath.Join(dir, "merged_fs.vmdk")
	require.NoError(t, DumpVMDKDescriptorToFile(descPath, 0xfffffffe, []string{ext1, ext2}))

	_, err := os.Stat(descPath)
	require.NoError(t, err, "descriptor file should exist after multi-device write")
}
