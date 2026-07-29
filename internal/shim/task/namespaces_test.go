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
	"errors"
	"reflect"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	nsAPI "github.com/containerd/nerdbox/api/services/namespaces/v1"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// Stand-in guest paths. The real values are chosen by the guest and returned
// over the wire, so the host must never assume a particular layout; these
// only need to be distinguishable from each other.
const (
	fakeNetPath = "/run/netns/test-sandbox"
	fakeIPCPath = "/run/ipcns/test-sandbox"
	fakePIDPath = "/run/pidns/test-sandbox"
)

// recorder is a sharedNamespacesFunc that serves fixed paths and records
// every request made through it, so tests can assert not just the resulting
// spec but which namespace types were actually asked of the guest.
type recorder struct {
	calls [][]nsAPI.NamespaceType
}

func (r *recorder) fn() sharedNamespacesFunc {
	return func(_ context.Context, types []nsAPI.NamespaceType) (map[nsAPI.NamespaceType]string, error) {
		r.calls = append(r.calls, types)
		out := make(map[nsAPI.NamespaceType]string, len(types))
		for _, t := range types {
			switch t {
			case nsAPI.NamespaceType_NAMESPACE_TYPE_NETWORK:
				out[t] = fakeNetPath
			case nsAPI.NamespaceType_NAMESPACE_TYPE_IPC:
				out[t] = fakeIPCPath
			case nsAPI.NamespaceType_NAMESPACE_TYPE_PID:
				out[t] = fakePIDPath
			}
		}
		return out, nil
	}
}

// requested flattens every recorded call into the list of types asked for. It
// also asserts the guest was called at most once, since sanitizeNamespaces is
// meant to batch its needs into a single request.
func (r *recorder) requested(t *testing.T) []nsAPI.NamespaceType {
	t.Helper()
	if len(r.calls) > 1 {
		t.Errorf("getSharedNS called %d times, want at most 1: %v", len(r.calls), r.calls)
	}
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[0]
}

func TestSanitizeNamespaces(t *testing.T) {
	ctx := context.Background()

	testcases := []struct {
		name            string
		linux           *specs.Linux
		hasDedicatedNIC bool
		want            []specs.LinuxNamespace
		// wantRequested is the exact set of namespace types the guest must be
		// asked for, in order. Nil means the guest must not be called at all.
		wantRequested []nsAPI.NamespaceType
	}{
		{
			name:  "nil Linux is a no-op",
			linux: nil,
			want:  nil,
		},
		{
			name:  "no namespaces, no dedicated NIC: shared network namespace added",
			linux: &specs.Linux{},
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: fakeNetPath},
			},
			wantRequested: []nsAPI.NamespaceType{nsAPI.NamespaceType_NAMESPACE_TYPE_NETWORK},
		},
		{
			name:            "no namespaces, dedicated NIC: nothing added, guest not called",
			linux:           &specs.Linux{},
			hasDedicatedNIC: true,
			want:            nil,
		},
		{
			name: "host network namespace path rewritten to the shared namespace",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.MountNamespace},
					{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
				},
			},
			want: []specs.LinuxNamespace{
				{Type: specs.MountNamespace},
				{Type: specs.NetworkNamespace, Path: fakeNetPath},
			},
			wantRequested: []nsAPI.NamespaceType{nsAPI.NamespaceType_NAMESPACE_TYPE_NETWORK},
		},
		{
			name: "dedicated NIC: existing network namespace path stripped (crun creates a fresh one)",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
				},
			},
			hasDedicatedNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: ""},
			},
		},
		{
			name: "host paths on UTS/User namespaces are stripped (no sharing mechanism for these)",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.UTSNamespace, Path: "/proc/12345/ns/uts"},
					{Type: specs.UserNamespace, Path: "/proc/12345/ns/user"},
				},
			},
			hasDedicatedNIC: true, // avoid also asserting the added network entry
			want: []specs.LinuxNamespace{
				{Type: specs.UTSNamespace, Path: ""},
				{Type: specs.UserNamespace, Path: ""},
			},
		},
		{
			name: "empty-Path network namespace with no dedicated NIC joins the shared namespace",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace},
				},
			},
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: fakeNetPath},
			},
			wantRequested: []nsAPI.NamespaceType{nsAPI.NamespaceType_NAMESPACE_TYPE_NETWORK},
		},
		{
			name: "host IPC namespace path redirected to the shared IPC namespace",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
				},
			},
			hasDedicatedNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: fakeIPCPath},
			},
			wantRequested: []nsAPI.NamespaceType{nsAPI.NamespaceType_NAMESPACE_TYPE_IPC},
		},
		{
			name: "host PID namespace path redirected to the shared PID namespace (covers both pod-level and node-level sharing)",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.PIDNamespace, Path: "/proc/12345/ns/pid"},
				},
			},
			hasDedicatedNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace, Path: fakePIDPath},
			},
			wantRequested: []nsAPI.NamespaceType{nsAPI.NamespaceType_NAMESPACE_TYPE_PID},
		},
		{
			name: "empty-Path IPC/PID namespaces (per-container mode) are left alone, guest not called",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.IPCNamespace},
					{Type: specs.PIDNamespace},
				},
			},
			hasDedicatedNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace},
				{Type: specs.PIDNamespace},
			},
		},
		{
			// A shared IPC namespace must not drag in a PID namespace. This
			// is the common CRI shape: Kubernetes shares pod IPC by default
			// but only shares PID when explicitly asked, and creating a PID
			// namespace costs the guest a persistent anchor process.
			name: "sharing IPC alone does not request a PID namespace",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
					{Type: specs.PIDNamespace},
				},
			},
			hasDedicatedNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: fakeIPCPath},
				{Type: specs.PIDNamespace},
			},
			wantRequested: []nsAPI.NamespaceType{nsAPI.NamespaceType_NAMESPACE_TYPE_IPC},
		},
		{
			name: "every shared namespace is requested in a single call",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
					{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
					{Type: specs.PIDNamespace, Path: "/proc/12345/ns/pid"},
				},
			},
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: fakeNetPath},
				{Type: specs.IPCNamespace, Path: fakeIPCPath},
				{Type: specs.PIDNamespace, Path: fakePIDPath},
			},
			wantRequested: []nsAPI.NamespaceType{
				nsAPI.NamespaceType_NAMESPACE_TYPE_NETWORK,
				nsAPI.NamespaceType_NAMESPACE_TYPE_IPC,
				nsAPI.NamespaceType_NAMESPACE_TYPE_PID,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder

			b := &bundle.Bundle{Spec: specs.Spec{Linux: tc.linux}}
			if err := sanitizeNamespaces(ctx, b, tc.hasDedicatedNIC, rec.fn()); err != nil {
				t.Fatalf("sanitizeNamespaces: %v", err)
			}

			var got []specs.LinuxNamespace
			if b.Spec.Linux != nil {
				got = b.Spec.Linux.Namespaces
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("namespaces = %+v, want %+v", got, tc.want)
			}
			if gotReq := rec.requested(t); !reflect.DeepEqual(gotReq, tc.wantRequested) {
				t.Errorf("requested namespace types = %v, want %v", gotReq, tc.wantRequested)
			}
		})
	}
}

// TestSanitizeNamespacesPropagatesSharedNSError verifies that a failure to
// obtain the shared namespaces (e.g. the guest RPC failing) is surfaced as an
// error, not silently ignored.
func TestSanitizeNamespacesPropagatesSharedNSError(t *testing.T) {
	b := &bundle.Bundle{Spec: specs.Spec{Linux: &specs.Linux{
		Namespaces: []specs.LinuxNamespace{
			{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
		},
	}}}
	wantErr := errors.New("guest unreachable")
	err := sanitizeNamespaces(context.Background(), b, true,
		func(context.Context, []nsAPI.NamespaceType) (map[nsAPI.NamespaceType]string, error) {
			return nil, wantErr
		})
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("sanitizeNamespaces error = %v, want wrapping %v", err, wantErr)
	}
}
