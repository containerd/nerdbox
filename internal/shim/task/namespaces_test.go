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

	srAPI "github.com/containerd/nerdbox/api/services/sharedresources/v1"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// Stand-in guest paths. The real values are chosen by the guest and returned
// over the wire, so the host must never assume a particular layout; these
// only need to be distinguishable from each other.
const (
	fakeNetPath = "/run/netns/test-sandbox"
	fakeIPCPath = "/run/ipcns/test-sandbox"
	fakeUTSPath = "/run/utsns/test-sandbox"
	fakePIDPath = "/run/pidns/test-sandbox"
)

// recorder is a sharedResourceFunc that serves fixed paths and records
// every request made through it, so tests can assert not just the resulting
// spec but which namespace types were actually asked of the guest.
type recorder struct {
	calls [][]srAPI.Type
}

func (r *recorder) fn() sharedResourceFunc {
	return func(_ context.Context, types []srAPI.Type) (map[srAPI.Type]string, error) {
		r.calls = append(r.calls, types)
		out := make(map[srAPI.Type]string, len(types))
		for _, t := range types {
			switch t {
			case srAPI.Type_TYPE_NAMESPACE_NETWORK:
				out[t] = fakeNetPath
			case srAPI.Type_TYPE_NAMESPACE_IPC:
				out[t] = fakeIPCPath
			case srAPI.Type_TYPE_NAMESPACE_UTS:
				out[t] = fakeUTSPath
			case srAPI.Type_TYPE_NAMESPACE_PID:
				out[t] = fakePIDPath
			}
		}
		return out, nil
	}
}

// requested flattens every recorded call into the list of types asked for. It
// also asserts the guest was called at most once, since sanitizeNamespaces is
// meant to batch its needs into a single request.
func (r *recorder) requested(t *testing.T) []srAPI.Type {
	t.Helper()
	if len(r.calls) > 1 {
		t.Errorf("getSharedResources called %d times, want at most 1: %v", len(r.calls), r.calls)
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
		hasContainerNIC bool
		want            []specs.LinuxNamespace
		// wantRequested is the exact set of namespace types the guest must be
		// asked for, in order. Nil means the guest must not be called at all.
		wantRequested []srAPI.Type
	}{
		{
			name:  "nil Linux is a no-op",
			linux: nil,
			want:  nil,
		},
		{
			name:            "container NIC: nothing added, guest not called",
			linux:           &specs.Linux{},
			hasContainerNIC: true,
			want:            nil,
		},
		{
			name: "no container NIC: host network namespace path dropped entirely",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.MountNamespace},
					{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
				},
			},
			want: []specs.LinuxNamespace{
				{Type: specs.MountNamespace},
			},
		},
		{
			name: "container NIC: existing network namespace path stripped (crun creates a fresh one)",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
				},
			},
			hasContainerNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: ""},
			},
		},
		{
			name: "host path on User namespace is stripped (no sharing mechanism for this type)",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.UserNamespace, Path: "/proc/12345/ns/user"},
				},
			},
			hasContainerNIC: true, // avoid also asserting the dropped network entry
			want: []specs.LinuxNamespace{
				{Type: specs.UserNamespace, Path: ""},
			},
		},
		{
			name: "host UTS namespace path redirected to the shared UTS namespace",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.UTSNamespace, Path: "/proc/12345/ns/uts"},
				},
			},
			hasContainerNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.UTSNamespace, Path: fakeUTSPath},
			},
			wantRequested: []srAPI.Type{srAPI.Type_TYPE_NAMESPACE_UTS},
		},
		{
			name: "empty-Path UTS namespace (per-container mode) is left alone, guest not called",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.UTSNamespace},
				},
			},
			hasContainerNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.UTSNamespace},
			},
		},
		{
			name: "no container NIC: empty-Path network namespace dropped too",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace},
				},
			},
			want: nil,
		},
		{
			name:  "no container NIC: no network namespace added",
			linux: &specs.Linux{},
			want:  nil,
		},
		{
			name: "container NIC: empty-Path network namespace keeps its own namespace",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace},
				},
			},
			hasContainerNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: ""},
			},
		},
		{
			name: "host IPC namespace path redirected to the shared IPC namespace",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
				},
			},
			hasContainerNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: fakeIPCPath},
			},
			wantRequested: []srAPI.Type{srAPI.Type_TYPE_NAMESPACE_IPC},
		},
		{
			name: "host PID namespace path redirected to the shared PID namespace (covers both pod-level and node-level sharing)",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.PIDNamespace, Path: "/proc/12345/ns/pid"},
				},
			},
			hasContainerNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace, Path: fakePIDPath},
			},
			wantRequested: []srAPI.Type{srAPI.Type_TYPE_NAMESPACE_PID},
		},
		{
			name: "empty-Path IPC/PID namespaces (per-container mode) are left alone, guest not called",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.IPCNamespace},
					{Type: specs.PIDNamespace},
				},
			},
			hasContainerNIC: true,
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
			hasContainerNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: fakeIPCPath},
				{Type: specs.PIDNamespace},
			},
			wantRequested: []srAPI.Type{srAPI.Type_TYPE_NAMESPACE_IPC},
		},
		{
			name: "every shared namespace is requested in a single call",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
					{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
					{Type: specs.UTSNamespace, Path: "/proc/12345/ns/uts"},
					{Type: specs.PIDNamespace, Path: "/proc/12345/ns/pid"},
				},
			},
			hasContainerNIC: true,
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: ""},
				{Type: specs.IPCNamespace, Path: fakeIPCPath},
				{Type: specs.UTSNamespace, Path: fakeUTSPath},
				{Type: specs.PIDNamespace, Path: fakePIDPath},
			},
			wantRequested: []srAPI.Type{
				srAPI.Type_TYPE_NAMESPACE_IPC,
				srAPI.Type_TYPE_NAMESPACE_UTS,
				srAPI.Type_TYPE_NAMESPACE_PID,
			},
		},
		{
			// Dropping the network namespace must not affect the others.
			name: "no container NIC: IPC sharing still works",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
					{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
				},
			},
			want: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: fakeIPCPath},
			},
			wantRequested: []srAPI.Type{srAPI.Type_TYPE_NAMESPACE_IPC},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder

			b := &bundle.Bundle{Spec: specs.Spec{Linux: tc.linux}}
			if err := sanitizeNamespaces(ctx, b, tc.hasContainerNIC, rec.fn()); err != nil {
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

// TestSanitizeNamespacesPropagatesSharedResourcesError verifies that a
// failure to obtain the shared namespaces (e.g. the guest RPC failing) is
// surfaced as an error, not silently ignored.
func TestSanitizeNamespacesPropagatesSharedResourcesError(t *testing.T) {
	b := &bundle.Bundle{Spec: specs.Spec{Linux: &specs.Linux{
		Namespaces: []specs.LinuxNamespace{
			{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
		},
	}}}
	wantErr := errors.New("guest unreachable")
	err := sanitizeNamespaces(context.Background(), b, true,
		func(context.Context, []srAPI.Type) (map[srAPI.Type]string, error) {
			return nil, wantErr
		})
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("sanitizeNamespaces error = %v, want wrapping %v", err, wantErr)
	}
}
