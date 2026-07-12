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

	"github.com/containerd/nerdbox/internal/podnetns"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// fakeSharedNS returns a sharedNamespacesFunc that always succeeds with
// the given fixed paths.
func fakeSharedNS(ipcPath, pidPath string) sharedNamespacesFunc {
	return func(context.Context) (string, string, error) {
		return ipcPath, pidPath, nil
	}
}

func TestSanitizeNamespaces(t *testing.T) {
	ctx := context.Background()

	testcases := []struct {
		name            string
		linux           *specs.Linux
		hasDedicatedNIC bool
		getSharedNS     sharedNamespacesFunc // nil: use a poison func that fails the test if called
		want            []specs.LinuxNamespace
	}{
		{
			name:  "nil Linux is a no-op",
			linux: nil,
			want:  nil,
		},
		{
			name:  "no namespaces, no dedicated NIC: network namespace added pointing at the shared pod netns",
			linux: &specs.Linux{},
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: podnetns.Path},
			},
		},
		{
			name:            "no namespaces, dedicated NIC: nothing added",
			linux:           &specs.Linux{},
			hasDedicatedNIC: true,
			want:            nil,
		},
		{
			name: "host network namespace path rewritten to the shared pod netns",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.MountNamespace},
					{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
				},
			},
			want: []specs.LinuxNamespace{
				{Type: specs.MountNamespace},
				{Type: specs.NetworkNamespace, Path: podnetns.Path},
			},
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
			name: "empty-Path network namespace with no dedicated NIC is rewritten to the shared pod netns",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.NetworkNamespace},
				},
			},
			want: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: podnetns.Path},
			},
		},
		{
			name: "host IPC namespace path redirected to the shared pod IPC namespace",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
				},
			},
			hasDedicatedNIC: true,
			getSharedNS:     fakeSharedNS("/run/ipcns/pod", "/run/pidns/pod"),
			want: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: "/run/ipcns/pod"},
			},
		},
		{
			name: "host PID namespace path redirected to the shared pod PID namespace (covers both PodPID and HostPID)",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.PIDNamespace, Path: "/proc/12345/ns/pid"},
				},
			},
			hasDedicatedNIC: true,
			getSharedNS:     fakeSharedNS("/run/ipcns/pod", "/run/pidns/pod"),
			want: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace, Path: "/run/pidns/pod"},
			},
		},
		{
			name: "empty-Path IPC/PID namespaces (NamespaceMode_CONTAINER) are left alone, no shared-namespace call made",
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
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			getSharedNS := tc.getSharedNS
			if getSharedNS == nil {
				getSharedNS = func(context.Context) (string, string, error) {
					t.Helper()
					t.Fatal("getSharedNS should not have been called")
					return "", "", nil
				}
			}

			b := &bundle.Bundle{Spec: specs.Spec{Linux: tc.linux}}
			if err := sanitizeNamespaces(ctx, b, tc.hasDedicatedNIC, getSharedNS); err != nil {
				t.Fatalf("sanitizeNamespaces: %v", err)
			}
			var got []specs.LinuxNamespace
			if b.Spec.Linux != nil {
				got = b.Spec.Linux.Namespaces
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("namespaces = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSanitizeNamespacesPropagatesSharedNSError verifies that a failure to
// obtain the shared namespaces (e.g. the guest RPC failing) is surfaced as
// an error, not silently ignored.
func TestSanitizeNamespacesPropagatesSharedNSError(t *testing.T) {
	b := &bundle.Bundle{Spec: specs.Spec{Linux: &specs.Linux{
		Namespaces: []specs.LinuxNamespace{
			{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
		},
	}}}
	wantErr := errors.New("guest unreachable")
	err := sanitizeNamespaces(context.Background(), b, true, func(context.Context) (string, string, error) {
		return "", "", wantErr
	})
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("sanitizeNamespaces error = %v, want wrapping %v", err, wantErr)
	}
}
