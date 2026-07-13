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

package libkrun

import (
	"testing"
)

// TestAddVirtiofs verifies that AddVirtiofs forwards the readonly flag to
// krun_add_virtiofs3.
func TestAddVirtiofs(t *testing.T) {
	type call struct {
		tag      string
		path     string
		readonly bool
	}
	var calls []call

	lib := &libkrun{
		AddVirtiofs3: func(ctxID uint32, tag, path string, shmSize uint64, readonly bool) int32 {
			calls = append(calls, call{tag, path, readonly})
			return 0
		},
	}
	vmc := &vmcontext{lib: lib}

	if err := vmc.AddVirtiofs("tag-ro", "/src/ro", true); err != nil {
		t.Fatalf("readonly call: unexpected error: %v", err)
	}
	if err := vmc.AddVirtiofs("tag-rw", "/src/rw", false); err != nil {
		t.Fatalf("read-write call: unexpected error: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0] != (call{tag: "tag-ro", path: "/src/ro", readonly: true}) {
		t.Fatalf("first call mismatch: %+v", calls[0])
	}
	if calls[1] != (call{tag: "tag-rw", path: "/src/rw", readonly: false}) {
		t.Fatalf("second call mismatch: %+v", calls[1])
	}
}

// TestAddVirtiofs_FailurePropagates ensures the libkrun return code from
// krun_add_virtiofs3 surfaces as an error.
func TestAddVirtiofs_FailurePropagates(t *testing.T) {
	lib := &libkrun{
		AddVirtiofs3: func(ctxID uint32, tag, path string, shmSize uint64, readonly bool) int32 {
			return -22
		},
	}
	vmc := &vmcontext{lib: lib}

	if err := vmc.AddVirtiofs("tag", "/p", true); err == nil {
		t.Fatalf("expected error when krun_add_virtiofs3 returns non-zero")
	}
}

// TestAddVirtiofs_LibraryNotLoaded verifies the early error when the
// virtiofs3 entry point is not bound (i.e. the library failed to load).
func TestAddVirtiofs_LibraryNotLoaded(t *testing.T) {
	vmc := &vmcontext{lib: &libkrun{}}
	if err := vmc.AddVirtiofs("tag", "/p", false); err == nil {
		t.Fatalf("expected error when AddVirtiofs3 is not bound")
	}
}
