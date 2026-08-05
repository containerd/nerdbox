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

// bundleArtifacts lists the entries of dir that IsBundleArtifact classifies as
// shim-written, as full paths — the same enumeration shim teardown performs.
func bundleArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var matched []string
	for _, e := range entries {
		if IsBundleArtifact(e.Name()) {
			matched = append(matched, filepath.Join(dir, e.Name()))
		}
	}
	return matched
}

// TestBundleArtifactsMatchWhatIsWritten is the anti-drift test: it writes the
// descriptors the shim actually writes into a bundle and asserts the
// classifier used by teardown matches every one of them. If a writer starts
// emitting a new name, this fails rather than silently leaving a file behind
// that would wedge the container id.
func TestBundleArtifactsMatchWhatIsWritten(t *testing.T) {
	t.Cleanup(func() {
		gptUseZeroExtents = true
	})
	// Disable ZERO extents so the pad file is written too, putting the full set
	// of auxiliary blobs in front of the classifier.
	gptUseZeroExtents = false

	bundle := t.TempDir()
	devices := []string{
		makeLayerFile(t, bundle, "a.img", 4*1024*1024),
		makeLayerFile(t, bundle, "b.img", 4*1024*1024),
	}

	// The GPT descriptor plus its auxiliary blobs.
	gptPath := filepath.Join(bundle, GPTDescriptorName)
	require.NoError(t, DumpGPTVMDKDescriptorToFile(gptPath, 0xfffffffe, devices))

	// A flat-concat descriptor, as written for a multi-device erofs mount.
	flatPath := filepath.Join(bundle, FlatDescriptorName('b'))
	require.NoError(t, DumpVMDKDescriptorToFile(flatPath, 0xfffffffe, devices))

	want := []string{
		gptPath,
		filepath.Join(bundle, "merged_fs_gpt_header.bin"),
		filepath.Join(bundle, "merged_fs_gpt_pad.bin"),
		flatPath,
	}
	for _, p := range want {
		require.FileExists(t, p, "expected the writers to produce this file")
	}

	matched := bundleArtifacts(t, bundle)

	assert.ElementsMatch(t, want, matched,
		"IsBundleArtifact must match exactly the descriptors and blobs written into the bundle")

	// Classification must not sweep up the layer images, which live outside
	// the bundle in production and are not the shim's to delete.
	for _, dev := range devices {
		assert.NotContains(t, matched, dev)
	}
}

// TestIsBundleArtifactWithGlobMetacharactersInPath guards the reason teardown
// enumerates instead of globbing: a bundle directory whose path contains a glob
// metacharacter must still have its artifacts found. filepath.Glob would return
// no matches and no error here, silently leaving the files that wedge the
// container id.
func TestIsBundleArtifactWithGlobMetacharactersInPath(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "state[1]")
	require.NoError(t, os.MkdirAll(bundle, 0o755))

	devices := []string{
		makeLayerFile(t, bundle, "a.img", 4*1024*1024),
		makeLayerFile(t, bundle, "b.img", 4*1024*1024),
	}
	gptPath := filepath.Join(bundle, GPTDescriptorName)
	require.NoError(t, DumpGPTVMDKDescriptorToFile(gptPath, 0xfffffffe, devices))

	assert.ElementsMatch(t,
		[]string{gptPath, filepath.Join(bundle, "merged_fs_gpt_header.bin")},
		bundleArtifacts(t, bundle))
}

func TestBundleArtifactsRemoveEverything(t *testing.T) {
	bundle := t.TempDir()
	devices := []string{
		makeLayerFile(t, bundle, "a.img", 4*1024*1024),
		makeLayerFile(t, bundle, "b.img", 4*1024*1024),
	}
	require.NoError(t, DumpGPTVMDKDescriptorToFile(
		filepath.Join(bundle, GPTDescriptorName), 0xfffffffe, devices))

	for _, p := range bundleArtifacts(t, bundle) {
		require.NoError(t, os.RemoveAll(p))
	}

	entries, err := os.ReadDir(bundle)
	require.NoError(t, err)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	assert.ElementsMatch(t, []string{"a.img", "b.img"}, left,
		"only the layer images should survive; every shim-written artifact must be gone")
}
