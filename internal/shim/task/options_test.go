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

package task

import (
	"context"
	"testing"

	runcOptions "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// marshalRuncOpts is a test helper that marshals runc Options into an *anypb.Any.
func marshalRuncOpts(t *testing.T, o *runcOptions.Options) *anypb.Any {
	t.Helper()
	a, err := typeurl.MarshalAny(o)
	if err != nil {
		t.Fatalf("marshalRuncOpts: %v", err)
	}
	return typeurl.MarshalProto(a)
}

// decodeRuncOpts is a test helper that decodes an *anypb.Any back to runc Options.
func decodeRuncOpts(t *testing.T, a *anypb.Any) *runcOptions.Options {
	t.Helper()
	v, err := typeurl.UnmarshalAny(a)
	if err != nil {
		t.Fatalf("decodeRuncOpts: %v", err)
	}
	out, ok := v.(*runcOptions.Options)
	if !ok {
		t.Fatalf("decodeRuncOpts: unexpected type %T", v)
	}
	return out
}

func TestGuestRuncOptions_NilOrEmpty(t *testing.T) {
	ctx := context.Background()

	// nil Any
	got, err := guestRuncOptions(ctx, nil)
	if err != nil {
		t.Fatalf("nil opts: unexpected error: %v", err)
	}
	out := decodeRuncOpts(t, got)
	if out.NoPivotRoot || out.NoNewKeyring {
		t.Errorf("nil opts: expected zero output, got %v", out)
	}

	// non-nil but empty value
	got, err = guestRuncOptions(ctx, &anypb.Any{})
	if err != nil {
		t.Fatalf("empty opts: unexpected error: %v", err)
	}
	out = decodeRuncOpts(t, got)
	if out.NoPivotRoot || out.NoNewKeyring {
		t.Errorf("empty opts: expected zero output, got %v", out)
	}
}

func TestGuestRuncOptions_AllowListedFieldsForwarded(t *testing.T) {
	ctx := context.Background()

	in := marshalRuncOpts(t, &runcOptions.Options{
		NoPivotRoot:  true,
		NoNewKeyring: true,
	})
	got, err := guestRuncOptions(ctx, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := decodeRuncOpts(t, got)
	if !out.NoPivotRoot {
		t.Error("NoPivotRoot should be forwarded")
	}
	if !out.NoNewKeyring {
		t.Error("NoNewKeyring should be forwarded")
	}
}

func TestGuestRuncOptions_HostFieldsStripped(t *testing.T) {
	ctx := context.Background()

	in := marshalRuncOpts(t, &runcOptions.Options{
		NoPivotRoot: true,
		// host-only fields
		Root:           "/run/containerd/runc",
		BinaryName:     "/usr/bin/runc",
		ShimCgroup:     "system.slice/containerd.service",
		IoUid:          1000,
		IoGid:          1000,
		TaskApiAddress: "/run/containerd/s/abc123",
	})
	got, err := guestRuncOptions(ctx, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := decodeRuncOpts(t, got)

	// Allow-listed field preserved.
	if !out.NoPivotRoot {
		t.Error("NoPivotRoot should be forwarded")
	}
	// Host-only fields must be zero.
	if out.Root != "" {
		t.Errorf("Root should be stripped, got %q", out.Root)
	}
	if out.BinaryName != "" {
		t.Errorf("BinaryName should be stripped, got %q", out.BinaryName)
	}
	if out.ShimCgroup != "" {
		t.Errorf("ShimCgroup should be stripped, got %q", out.ShimCgroup)
	}
	if out.IoUid != 0 {
		t.Errorf("IoUid should be stripped, got %d", out.IoUid)
	}
	if out.IoGid != 0 {
		t.Errorf("IoGid should be stripped, got %d", out.IoGid)
	}
	if out.TaskApiAddress != "" {
		t.Errorf("TaskApiAddress should be stripped, got %q", out.TaskApiAddress)
	}
}

func TestGuestRuncOptions_WarnedFieldsStripped(t *testing.T) {
	ctx := context.Background()

	in := marshalRuncOpts(t, &runcOptions.Options{
		SystemdCgroup: true,
		CriuImagePath: "/host/criu/images",
		CriuWorkPath:  "/host/criu/work",
	})
	got, err := guestRuncOptions(ctx, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := decodeRuncOpts(t, got)

	if out.SystemdCgroup {
		t.Error("SystemdCgroup should not be forwarded to guest")
	}
	if out.CriuImagePath != "" {
		t.Errorf("CriuImagePath should not be forwarded, got %q", out.CriuImagePath)
	}
	if out.CriuWorkPath != "" {
		t.Errorf("CriuWorkPath should not be forwarded, got %q", out.CriuWorkPath)
	}
}

func TestGuestRuncOptions_UnknownTypeReturnsError(t *testing.T) {
	ctx := context.Background()

	// Craft an Any with a foreign type URL to simulate a non-runc options type.
	a := &anypb.Any{
		TypeUrl: "type.googleapis.com/some.other.Options",
		Value:   []byte{0x0a, 0x03, 0x66, 0x6f, 0x6f}, // arbitrary bytes
	}
	_, err := guestRuncOptions(ctx, a)
	if err == nil {
		t.Fatal("expected error for unknown options type, got nil")
	}
}

func TestGuestRuncOptions_FreshInstanceEachCall(t *testing.T) {
	ctx := context.Background()

	in := marshalRuncOpts(t, &runcOptions.Options{NoPivotRoot: true})

	got1, err := guestRuncOptions(ctx, in)
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	got2, err := guestRuncOptions(ctx, in)
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}

	// The two results should be equal in content but must be distinct allocations.
	if got1 == got2 {
		t.Error("guestRuncOptions should return a new *Any each call")
	}
	out1 := decodeRuncOpts(t, got1)
	out2 := decodeRuncOpts(t, got2)
	if !proto.Equal(out1, out2) {
		t.Error("two calls with same input should produce equal output")
	}
}
