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
	"context"
	"testing"
)

func TestParseCTag(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOpt  bool
	}{
		{"krun_create_ctx", "krun_create_ctx", false},
		{"krun_add_virtiofs3,optional", "krun_add_virtiofs3", true},
		{"krun_foo,optional,bogus", "krun_foo", true},
		{"krun_foo,bogus", "krun_foo", false},
		{"", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			name, opt := parseCTag(c.in)
			if name != c.wantName || opt != c.wantOpt {
				t.Fatalf("parseCTag(%q) = (%q, %v), want (%q, %v)", c.in, name, opt, c.wantName, c.wantOpt)
			}
		})
	}
}

// TestAddVirtiofs_PrefersV3 verifies that when libkrun exports
// krun_add_virtiofs3, AddVirtiofs routes through it and forwards the
// readonly flag without falling back to the legacy entry point.
func TestAddVirtiofs_PrefersV3(t *testing.T) {
	type v3Call struct {
		tag      string
		path     string
		readonly bool
	}
	var v3Calls []v3Call
	legacyCalled := false

	lib := &libkrun{
		AddVirtiofs3: func(ctxID uint32, tag, path string, shmSize uint64, readonly bool) int32 {
			v3Calls = append(v3Calls, v3Call{tag, path, readonly})
			return 0
		},
		AddVirtiofs: func(ctxID uint32, tag, path string) int32 {
			legacyCalled = true
			return 0
		},
	}
	vmc := &vmcontext{lib: lib}

	if err := vmc.AddVirtiofs(context.Background(), "tag-ro", "/src/ro", true); err != nil {
		t.Fatalf("readonly call: unexpected error: %v", err)
	}
	if err := vmc.AddVirtiofs(context.Background(), "tag-rw", "/src/rw", false); err != nil {
		t.Fatalf("read-write call: unexpected error: %v", err)
	}

	if legacyCalled {
		t.Fatalf("legacy AddVirtiofs was invoked while AddVirtiofs3 was available")
	}
	if len(v3Calls) != 2 {
		t.Fatalf("expected 2 v3 calls, got %d", len(v3Calls))
	}
	if v3Calls[0] != (v3Call{tag: "tag-ro", path: "/src/ro", readonly: true}) {
		t.Fatalf("first v3 call mismatch: %+v", v3Calls[0])
	}
	if v3Calls[1] != (v3Call{tag: "tag-rw", path: "/src/rw", readonly: false}) {
		t.Fatalf("second v3 call mismatch: %+v", v3Calls[1])
	}
}

// TestAddVirtiofs_FallsBackWhenV3Missing verifies that when the loaded
// libkrun does not export krun_add_virtiofs3, AddVirtiofs falls back to
// the legacy entry point. The readonly flag cannot be carried at the
// host edge in that case but the call must still succeed; enforcement
// is left to the guest mount options.
func TestAddVirtiofs_FallsBackWhenV3Missing(t *testing.T) {
	type legacyCall struct {
		tag  string
		path string
	}
	var legacyCalls []legacyCall

	lib := &libkrun{
		// AddVirtiofs3 intentionally nil.
		AddVirtiofs: func(ctxID uint32, tag, path string) int32 {
			legacyCalls = append(legacyCalls, legacyCall{tag, path})
			return 0
		},
	}
	vmc := &vmcontext{lib: lib}

	if err := vmc.AddVirtiofs(context.Background(), "tag-ro", "/src/ro", true); err != nil {
		t.Fatalf("readonly fallback: unexpected error: %v", err)
	}
	if err := vmc.AddVirtiofs(context.Background(), "tag-rw", "/src/rw", false); err != nil {
		t.Fatalf("read-write fallback: unexpected error: %v", err)
	}

	if len(legacyCalls) != 2 {
		t.Fatalf("expected 2 legacy calls, got %d", len(legacyCalls))
	}
	if legacyCalls[0] != (legacyCall{tag: "tag-ro", path: "/src/ro"}) {
		t.Fatalf("first legacy call mismatch: %+v", legacyCalls[0])
	}
	if legacyCalls[1] != (legacyCall{tag: "tag-rw", path: "/src/rw"}) {
		t.Fatalf("second legacy call mismatch: %+v", legacyCalls[1])
	}
}

// TestAddVirtiofs_V3FailurePropagates ensures the libkrun return code
// from krun_add_virtiofs3 surfaces as an error and is not silently
// downgraded to the legacy path.
func TestAddVirtiofs_V3FailurePropagates(t *testing.T) {
	legacyCalled := false
	lib := &libkrun{
		AddVirtiofs3: func(ctxID uint32, tag, path string, shmSize uint64, readonly bool) int32 {
			return -22
		},
		AddVirtiofs: func(ctxID uint32, tag, path string) int32 {
			legacyCalled = true
			return 0
		},
	}
	vmc := &vmcontext{lib: lib}

	err := vmc.AddVirtiofs(context.Background(), "tag", "/p", true)
	if err == nil {
		t.Fatalf("expected error when krun_add_virtiofs3 returns non-zero")
	}
	if legacyCalled {
		t.Fatalf("legacy AddVirtiofs must not be tried after krun_add_virtiofs3 failure")
	}
}

// TestAddVirtiofs_LegacyFailurePropagates ensures the libkrun return code
// from the legacy krun_add_virtiofs surfaces as an error in the fallback
// path.
func TestAddVirtiofs_LegacyFailurePropagates(t *testing.T) {
	lib := &libkrun{
		AddVirtiofs: func(ctxID uint32, tag, path string) int32 {
			return -1
		},
	}
	vmc := &vmcontext{lib: lib}

	err := vmc.AddVirtiofs(context.Background(), "tag", "/p", false)
	if err == nil {
		t.Fatalf("expected error when krun_add_virtiofs returns non-zero")
	}
}

// TestAddVirtiofs_LibraryNotLoaded verifies the early error when neither
// virtiofs entry point is bound (i.e. the library failed to load).
func TestAddVirtiofs_LibraryNotLoaded(t *testing.T) {
	vmc := &vmcontext{lib: &libkrun{}}
	if err := vmc.AddVirtiofs(context.Background(), "tag", "/p", false); err == nil {
		t.Fatalf("expected error when neither AddVirtiofs nor AddVirtiofs3 is bound")
	}
}
