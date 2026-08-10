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

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func TestDevShmSize(t *testing.T) {
	testcases := []struct {
		name string
		opts []string
		want int64
	}{
		{name: "no options", opts: nil, want: defaultDevShmSize},
		{name: "no size option", opts: []string{"nosuid", "noexec"}, want: defaultDevShmSize},
		{name: "the exact form containerd sends", opts: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}, want: 65536 * 1024},
		{name: "kilobytes lowercase", opts: []string{"size=1024k"}, want: 1024 * 1024},
		{name: "kilobytes uppercase suffix", opts: []string{"size=1024K"}, want: 1024 * 1024},
		{name: "megabytes", opts: []string{"size=64m"}, want: 64 * 1024 * 1024},
		{name: "gigabytes", opts: []string{"size=1g"}, want: 1024 * 1024 * 1024},
		{name: "plain byte count, no suffix", opts: []string{"size=8192"}, want: 8192},
		{name: "unparseable value falls back", opts: []string{"size=50%"}, want: defaultDevShmSize},
		{name: "empty value falls back", opts: []string{"size="}, want: defaultDevShmSize},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, devShmSize(tc.opts))
		})
	}
}

func TestContainerSharesIPC(t *testing.T) {
	testcases := []struct {
		name  string
		linux *specs.Linux
		want  bool
	}{
		{name: "nil Linux", linux: nil, want: false},
		{name: "no namespaces", linux: &specs.Linux{}, want: false},
		{
			name: "empty-Path IPC namespace (not sharing)",
			linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace},
			}},
			want: false,
		},
		{
			name: "host-path IPC namespace (sharing requested)",
			linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
			}},
			want: true,
		},
		{
			name: "already-rewritten guest IPC path still counts as sharing",
			linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: "/run/ipcns/some-sandbox"},
			}},
			want: true,
		},
		{
			name: "other namespace types don't count",
			linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: "/proc/12345/ns/net"},
				{Type: specs.PIDNamespace, Path: "/proc/12345/ns/pid"},
			}},
			want: false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			b := &bundle.Bundle{Spec: specs.Spec{Linux: tc.linux}}
			assert.Equal(t, tc.want, containerSharesIPC(b))
		})
	}
}

func TestShareDevShmMounter_FromBundle(t *testing.T) {
	ctx := context.Background()

	t.Run("no IPC sharing: mount left alone, getDevShm not called", func(t *testing.T) {
		var calls int
		b := &bundle.Bundle{Spec: specs.Spec{
			Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace}, // empty Path: not sharing
			}},
			Mounts: []specs.Mount{
				{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"size=65536k"}},
			},
		}}

		d := &shareDevShmMounter{containerID: "ctr-1"}
		d.getDevShmFn = func(context.Context, int64) (string, error) {
			calls++
			return "/should/not/be/used", nil
		}
		require.NoError(t, d.FromBundle(ctx, b))
		assert.Equal(t, 0, calls)
		assert.Equal(t, "tmpfs", b.Spec.Mounts[0].Type, "mount must be left untouched when not sharing IPC")
	})

	t.Run("IPC sharing with a plain tmpfs /dev/shm: rewritten to bind", func(t *testing.T) {
		var gotSize int64
		b := &bundle.Bundle{Spec: specs.Spec{
			Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
			}},
			Mounts: []specs.Mount{
				{Destination: "/proc", Type: "proc", Source: "proc"},
				{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			},
		}}

		d := &shareDevShmMounter{containerID: "ctr-1"}
		d.getDevShmFn = func(_ context.Context, sizeBytes int64) (string, error) {
			gotSize = sizeBytes
			return "/run/devshm/some-sandbox", nil
		}
		require.NoError(t, d.FromBundle(ctx, b))

		assert.Equal(t, int64(65536*1024), gotSize)
		assert.Equal(t, specs.Mount{Destination: "/proc", Type: "proc", Source: "proc"}, b.Spec.Mounts[0],
			"unrelated mounts must be untouched")
		assert.Equal(t, specs.Mount{
			Destination: "/dev/shm",
			Type:        "bind",
			Source:      "/run/devshm/some-sandbox",
			Options:     []string{"rbind"},
		}, b.Spec.Mounts[1])
	})

	t.Run("already a bind mount: left alone (idempotent / caller-overridden)", func(t *testing.T) {
		var calls int
		b := &bundle.Bundle{Spec: specs.Spec{
			Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
			}},
			Mounts: []specs.Mount{
				{Destination: "/dev/shm", Type: "bind", Source: "/already/shared", Options: []string{"rbind"}},
			},
		}}

		d := &shareDevShmMounter{containerID: "ctr-1"}
		d.getDevShmFn = func(context.Context, int64) (string, error) {
			calls++
			return "/should/not/be/used", nil
		}
		require.NoError(t, d.FromBundle(ctx, b))
		assert.Equal(t, 0, calls)
		assert.Equal(t, "/already/shared", b.Spec.Mounts[0].Source)
	})

	t.Run("no /dev/shm mount present: no-op", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{
			Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
			}},
			Mounts: []specs.Mount{
				{Destination: "/proc", Type: "proc", Source: "proc"},
			},
		}}

		d := &shareDevShmMounter{containerID: "ctr-1"}
		d.getDevShmFn = func(context.Context, int64) (string, error) {
			t.Fatal("must not be called: no /dev/shm mount present")
			return "", nil
		}
		require.NoError(t, d.FromBundle(ctx, b))
	})

	t.Run("getDevShm failure propagates", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{
			Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.IPCNamespace, Path: "/proc/12345/ns/ipc"},
			}},
			Mounts: []specs.Mount{
				{Destination: "/dev/shm", Type: "tmpfs", Source: "shm"},
			},
		}}

		d := &shareDevShmMounter{containerID: "ctr-1"}
		d.getDevShmFn = func(context.Context, int64) (string, error) {
			return "", assert.AnError
		}
		err := d.FromBundle(ctx, b)
		require.Error(t, err)
		assert.ErrorIs(t, err, assert.AnError)
	})
}
