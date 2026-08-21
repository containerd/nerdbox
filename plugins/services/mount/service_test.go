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

package mount

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	api "github.com/containerd/nerdbox/api/services/mount/v1"
)

// newTestService builds a service with fake mounted/doMount functions, so
// MountAll's reconcile logic can be exercised deterministically without a
// privileged real mount.
func newTestService(mounted func(string) (bool, error), doMount func(*api.MountSpec) error) *service {
	if doMount == nil {
		doMount = func(*api.MountSpec) error { return nil }
	}
	return &service{mounted: mounted, doMount: doMount}
}

func mountSpec(target string) *api.MountSpec {
	return &api.MountSpec{Type: "bind", Source: "/src", Target: target, Options: []string{"bind"}}
}

// TestMountAllSkipsWhenReallyMounted verifies the common case: bookkeeping
// says the target is mounted with a matching spec, and it genuinely still
// is, so MountAll skips it without calling doMount again.
func TestMountAllSkipsWhenReallyMounted(t *testing.T) {
	var doMountCalls int
	s := newTestService(
		func(string) (bool, error) { return true, nil },
		func(*api.MountSpec) error { doMountCalls++; return nil },
	)
	s.mounts = []*api.MountSpec{mountSpec("/target")}

	if _, err := s.MountAll(context.Background(), &api.MountAllRequest{Mounts: []*api.MountSpec{mountSpec("/target")}}); err != nil {
		t.Fatalf("MountAll: %v", err)
	}
	if doMountCalls != 0 {
		t.Errorf("doMount called %d times, want 0 (should have been skipped)", doMountCalls)
	}
	if len(s.mounts) != 1 {
		t.Errorf("s.mounts = %v, want 1 entry retained", s.mounts)
	}
}

// TestMountAllReconcilesStaleBookkeeping verifies that when bookkeeping
// claims a target is mounted but it genuinely is not (e.g. unmounted by
// other means, or the guest mount service restarted), MountAll drops the
// stale entry and mounts fresh instead of trusting the record.
func TestMountAllReconcilesStaleBookkeeping(t *testing.T) {
	var doMountCalls int
	s := newTestService(
		func(string) (bool, error) { return false, nil }, // not really mounted
		func(*api.MountSpec) error { doMountCalls++; return nil },
	)
	s.mounts = []*api.MountSpec{mountSpec("/target")}

	if _, err := s.MountAll(context.Background(), &api.MountAllRequest{Mounts: []*api.MountSpec{mountSpec("/target")}}); err != nil {
		t.Fatalf("MountAll: %v", err)
	}
	if doMountCalls != 1 {
		t.Errorf("doMount called %d times, want 1 (stale entry should have been remounted)", doMountCalls)
	}
	if len(s.mounts) != 1 {
		t.Errorf("s.mounts = %v, want exactly 1 entry after reconcile+remount", s.mounts)
	}
}

// TestMountAllReconcilesTargetGone verifies that mounted returning
// fs.ErrNotExist (the target directory itself no longer exists) is treated
// the same as "not mounted", not as a fatal error.
func TestMountAllReconcilesTargetGone(t *testing.T) {
	var doMountCalls int
	s := newTestService(
		func(string) (bool, error) { return false, fs.ErrNotExist },
		func(*api.MountSpec) error { doMountCalls++; return nil },
	)
	s.mounts = []*api.MountSpec{mountSpec("/target")}

	if _, err := s.MountAll(context.Background(), &api.MountAllRequest{Mounts: []*api.MountSpec{mountSpec("/target")}}); err != nil {
		t.Fatalf("MountAll: %v", err)
	}
	if doMountCalls != 1 {
		t.Errorf("doMount called %d times, want 1", doMountCalls)
	}
}

// TestMountAllDifferentSpecStillMounted verifies that a target genuinely
// still mounted, but with a different spec than requested, is still
// rejected as a conflict rather than silently remounted or skipped.
func TestMountAllDifferentSpecStillMounted(t *testing.T) {
	s := newTestService(func(string) (bool, error) { return true, nil }, nil)
	s.mounts = []*api.MountSpec{mountSpec("/target")}

	different := mountSpec("/target")
	different.Source = "/other-src"

	_, err := s.MountAll(context.Background(), &api.MountAllRequest{Mounts: []*api.MountSpec{different}})
	if err == nil {
		t.Fatal("MountAll: got nil error, want a conflict error")
	}
	if !strings.Contains(err.Error(), "already mounted with a different spec") {
		t.Errorf("MountAll error = %v, want a spec-conflict error", err)
	}
}

// TestMountAllReconcileErrorPropagates verifies that a real (non-"missing")
// error from the mounted check is surfaced, not silently treated as "not
// mounted".
func TestMountAllReconcileErrorPropagates(t *testing.T) {
	wantErr := errors.New("mountinfo read failed")
	s := newTestService(func(string) (bool, error) { return false, wantErr }, nil)
	s.mounts = []*api.MountSpec{mountSpec("/target")}

	_, err := s.MountAll(context.Background(), &api.MountAllRequest{Mounts: []*api.MountSpec{mountSpec("/target")}})
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("MountAll error = %v, want it to mention %q", err, wantErr)
	}
}

// TestMountAllNewTargetNoReconcile verifies a target with no bookkeeping
// entry at all is mounted directly, without ever consulting mounted.
func TestMountAllNewTargetNoReconcile(t *testing.T) {
	var mountedCalls, doMountCalls int
	s := newTestService(
		func(string) (bool, error) { mountedCalls++; return true, nil },
		func(*api.MountSpec) error { doMountCalls++; return nil },
	)

	if _, err := s.MountAll(context.Background(), &api.MountAllRequest{Mounts: []*api.MountSpec{mountSpec("/target")}}); err != nil {
		t.Fatalf("MountAll: %v", err)
	}
	if mountedCalls != 0 {
		t.Errorf("mounted called %d times, want 0 (no prior bookkeeping to reconcile)", mountedCalls)
	}
	if doMountCalls != 1 {
		t.Errorf("doMount called %d times, want 1", doMountCalls)
	}
	if len(s.mounts) != 1 {
		t.Errorf("s.mounts = %v, want 1 entry", s.mounts)
	}
}

// TestUnmountToleratesTargetAlreadyGone verifies that Unmount does not fail
// when the target directory no longer exists at all (e.g. removed by
// cleanup racing this call), since there is nothing left to unmount.
func TestUnmountToleratesTargetAlreadyGone(t *testing.T) {
	// Use a target that both looks unmounted and does not exist, so the
	// real ctrMount.Unmount call this test exercises (doMount/mounted
	// fakes only affect MountAll) hits ENOENT rather than EINVAL.
	target := t.TempDir() + "/does-not-exist"

	s := newTestService(nil, nil)
	s.mounts = []*api.MountSpec{mountSpec(target)}

	if _, err := s.Unmount(context.Background(), &api.UnmountRequest{Target: target}); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if len(s.mounts) != 0 {
		t.Errorf("s.mounts = %v, want empty after Unmount", s.mounts)
	}
}

// TestUnmountAllToleratesTargetAlreadyGone is the UnmountAll analogue of
// TestUnmountToleratesTargetAlreadyGone.
func TestUnmountAllToleratesTargetAlreadyGone(t *testing.T) {
	target := t.TempDir() + "/does-not-exist"

	s := newTestService(nil, nil)
	s.mounts = []*api.MountSpec{mountSpec(target)}

	if _, err := s.UnmountAll(context.Background(), &api.UnmountAllRequest{}); err != nil {
		t.Fatalf("UnmountAll: %v", err)
	}
	if len(s.mounts) != 0 {
		t.Errorf("s.mounts = %v, want empty after UnmountAll", s.mounts)
	}
}

// TestUnmountUnknownTarget verifies Unmount still rejects a target with no
// bookkeeping entry at all: "tolerant of already-gone" only applies to
// something this service actually thought it had mounted.
func TestUnmountUnknownTarget(t *testing.T) {
	s := newTestService(nil, nil)

	_, err := s.Unmount(context.Background(), &api.UnmountRequest{Target: "/never-mounted"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Unmount error = %v, want a not-found error", err)
	}
}
