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

import "testing"

// TestResolveNetNS_FirstCallRecords verifies that the first call records
// whatever namespace (including empty, i.e. host-network) was requested.
func TestResolveNetNS_FirstCallRecords(t *testing.T) {
	v := &vmInstance{}
	if err := v.resolveNetNS("/run/netns/foo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.netnsSet || v.netns != "/run/netns/foo" {
		t.Fatalf("netns not recorded: netnsSet=%v netns=%q", v.netnsSet, v.netns)
	}
}

// TestResolveNetNS_SameIsNoop verifies that repeating the same namespace
// after it was already recorded succeeds without changing anything.
func TestResolveNetNS_SameIsNoop(t *testing.T) {
	v := &vmInstance{}
	if err := v.resolveNetNS("/run/netns/foo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := v.resolveNetNS("/run/netns/foo"); err != nil {
		t.Fatalf("repeating the same netns should be a no-op, got error: %v", err)
	}
	if v.netns != "/run/netns/foo" {
		t.Fatalf("netns changed unexpectedly: %q", v.netns)
	}
}

// TestResolveNetNS_EmptyIsNoop verifies that an empty (host-network) request
// after a real namespace was already recorded is ignored rather than
// clearing the recorded namespace.
func TestResolveNetNS_EmptyIsNoop(t *testing.T) {
	v := &vmInstance{}
	if err := v.resolveNetNS("/run/netns/foo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := v.resolveNetNS(""); err != nil {
		t.Fatalf("an empty netns request should be a no-op, got error: %v", err)
	}
	if v.netns != "/run/netns/foo" {
		t.Fatalf("netns cleared unexpectedly: %q", v.netns)
	}
}

// TestResolveNetNS_ConflictErrors verifies that a genuinely different,
// non-empty namespace after one was already recorded is rejected.
func TestResolveNetNS_ConflictErrors(t *testing.T) {
	v := &vmInstance{}
	if err := v.resolveNetNS("/run/netns/foo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := v.resolveNetNS("/run/netns/bar"); err == nil {
		t.Fatalf("expected error when requesting a different netns")
	}
	if v.netns != "/run/netns/foo" {
		t.Fatalf("netns changed despite conflict: %q", v.netns)
	}
}

// TestResolveNetNS_EmptyFirstThenNonEmptyErrors verifies that a namespace
// requested after host-network was already recorded (the empty string) is
// treated as a genuine conflict, not a no-op.
func TestResolveNetNS_EmptyFirstThenNonEmptyErrors(t *testing.T) {
	v := &vmInstance{}
	if err := v.resolveNetNS(""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := v.resolveNetNS("/run/netns/foo"); err == nil {
		t.Fatalf("expected error when requesting a netns after host-network was recorded")
	}
}
