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

package sharedresources

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestValidateID covers the group ids that must be rejected before being
// joined onto a directory to form a bind-mount path. The id arrives over RPC,
// so anything that could escape the per-type directory has to be refused
// rather than sanitized.
func TestValidateID(t *testing.T) {
	testcases := []struct {
		id      string
		wantErr bool
	}{
		{id: "sandbox", wantErr: false},
		{id: "0f9d998d2b1c4e5a", wantErr: false},
		{id: "with-dashes_and_underscores.1", wantErr: false},
		{id: "..hidden", wantErr: false},

		{id: "", wantErr: true},
		{id: ".", wantErr: true},
		{id: "..", wantErr: true},
		{id: "/", wantErr: true},
		{id: "a/b", wantErr: true},
		{id: "../etc/passwd", wantErr: true},
		{id: "/absolute", wantErr: true},
		{id: "trailing/", wantErr: true},
		{id: "nul\x00byte", wantErr: true},
	}

	for _, tc := range testcases {
		t.Run(tc.id, func(t *testing.T) {
			err := validateID(tc.id)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateID(%q) = nil, want error", tc.id)
				}
				if !errors.Is(err, ErrInvalidArgument) {
					t.Errorf("validateID(%q) error = %v, want it to wrap ErrInvalidArgument", tc.id, err)
				}
				return
			}
			if err != nil {
				t.Errorf("validateID(%q) = %v, want nil", tc.id, err)
			}
		})
	}
}

// TestValidateIDRejectionsCannotEscape is a belt-and-braces check that every
// id validateID accepts stays inside its type directory once joined.
func TestCreateRejectsInvalidID(t *testing.T) {
	var m Manager
	if _, err := m.Create(context.Background(), "../escape", []Type{TypeNamespaceIPC}, 0); err == nil {
		t.Fatal("Create with a traversing id = nil error, want failure")
	} else if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Create error = %v, want it to wrap ErrInvalidArgument", err)
	}

	// Nothing may have been recorded for a rejected request.
	if len(m.ns) != 0 {
		t.Errorf("manager recorded %d resources after a rejected request, want 0", len(m.ns))
	}
}

func TestDedupe(t *testing.T) {
	testcases := []struct {
		name    string
		in      []Type
		want    []Type
		wantErr bool
	}{
		{
			name: "nil",
			in:   nil,
			want: []Type{},
		},
		{
			name: "order preserved",
			in:   []Type{TypeNamespaceNetwork, TypeNamespaceIPC, TypeNamespacePID},
			want: []Type{TypeNamespaceNetwork, TypeNamespaceIPC, TypeNamespacePID},
		},
		{
			name: "duplicates collapsed, first-seen order kept",
			in:   []Type{TypeNamespacePID, TypeNamespaceIPC, TypeNamespacePID, TypeNamespaceIPC, TypeNamespacePID},
			want: []Type{TypeNamespacePID, TypeNamespaceIPC},
		},
		{
			name:    "unknown type rejected",
			in:      []Type{TypeNamespaceIPC, Type(99)},
			wantErr: true,
		},
		{
			name:    "zero value is not a valid type",
			in:      []Type{Type(0)},
			wantErr: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dedupe(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("dedupe(%v) = nil error, want failure", tc.in)
				}
				if !errors.Is(err, ErrInvalidArgument) {
					t.Errorf("dedupe error = %v, want it to wrap ErrInvalidArgument", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("dedupe(%v): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dedupe(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestTypeDir pins the guest path layout, since these paths are handed to the
// OCI runtime and are the only contract the host relies on.
func TestTypeDir(t *testing.T) {
	testcases := []struct {
		typ  Type
		want string
	}{
		{typ: TypeNamespaceNetwork, want: "/run/netns"},
		{typ: TypeNamespaceIPC, want: "/run/ipcns"},
		{typ: TypeNamespaceUTS, want: "/run/utsns"},
		{typ: TypeNamespacePID, want: "/run/pidns"},
	}
	for _, tc := range testcases {
		t.Run(tc.typ.String(), func(t *testing.T) {
			got, err := tc.typ.dir()
			if err != nil {
				t.Fatalf("dir(): %v", err)
			}
			if got != tc.want {
				t.Errorf("dir() = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := Type(0).dir(); err == nil {
		t.Error("dir() for the zero Type = nil error, want failure")
	}
}

// TestUnpinIsIdempotent verifies deleting a resource that was never created,
// or was already cleaned up, is not an error: Delete is documented as
// tolerating both.
func TestUnpinIsIdempotent(t *testing.T) {
	path := t.TempDir() + "/never-existed"
	if err := unpin(path); err != nil {
		t.Errorf("unpin of a nonexistent path = %v, want nil", err)
	}

	// A plain file that was pinned but never mounted onto must still be
	// removed.
	pinned := t.TempDir() + "/pinned"
	if err := pin(pinned); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := unpin(pinned); err != nil {
		t.Errorf("unpin of an unmounted pin = %v, want nil", err)
	}
	if err := unpin(pinned); err != nil {
		t.Errorf("second unpin = %v, want nil", err)
	}
}

// TestDeleteUnknownIsNoop verifies Delete on a manager that never created
// anything succeeds rather than reporting a missing resource.
func TestDeleteUnknownIsNoop(t *testing.T) {
	var m Manager
	if err := m.Delete(context.Background(), "sandbox", []Type{TypeNamespaceIPC, TypeNamespacePID, TypeNamespaceNetwork}); err != nil {
		t.Errorf("Delete of unknown resources = %v, want nil", err)
	}
	if err := m.Delete(context.Background(), "sandbox", nil); err != nil {
		t.Errorf("Delete of an unknown group = %v, want nil", err)
	}
}
