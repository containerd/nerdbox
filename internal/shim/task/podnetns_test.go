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
	"reflect"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/nerdbox/internal/podnetns"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func TestSanitizeNamespaces(t *testing.T) {
	ctx := context.Background()

	testcases := []struct {
		name            string
		linux           *specs.Linux
		hasDedicatedNIC bool
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
			name: "host paths on any other namespace type are stripped",
			linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.PIDNamespace, Path: "/proc/12345/ns/pid"},
					{Type: specs.UTSNamespace, Path: "/proc/12345/ns/uts"},
					{Type: specs.UserNamespace, Path: "/proc/12345/ns/user"},
				},
			},
			hasDedicatedNIC: true, // avoid also asserting the added network entry
			want: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace, Path: ""},
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
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			b := &bundle.Bundle{Spec: specs.Spec{Linux: tc.linux}}
			if err := sanitizeNamespaces(ctx, b, tc.hasDedicatedNIC); err != nil {
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
