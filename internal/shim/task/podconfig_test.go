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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/containerd/nerdbox/api/types"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// marshalTestPodSandboxConfig marshals cfg with the real
// google.golang.org/protobuf/proto encoder, then wraps it with the actual
// type URL a real CreateSandboxRequest.Options carries, so these tests
// exercise the real decode path (decodePodSandboxConfig) exactly as a real
// CRI-originated payload would.
func marshalTestPodSandboxConfig(t *testing.T, cfg *types.CRIPodConfig) *anypb.Any {
	t.Helper()
	data, err := proto.Marshal(cfg)
	require.NoError(t, err)
	return &anypb.Any{TypeUrl: podSandboxConfigTypeURL, Value: data}
}

func TestPodSandboxConfig(t *testing.T) {
	t.Run("nil options is a no-op, not an error", func(t *testing.T) {
		cfg, err := decodePodSandboxConfig(nil)
		require.NoError(t, err)
		assert.Nil(t, cfg)
	})

	t.Run("decodes a real PodSandboxConfig", func(t *testing.T) {
		opts := marshalTestPodSandboxConfig(t, &types.CRIPodConfig{
			Hostname: "my-pod",
			DnsConfig: &types.CRIDNSConfig{
				Servers: []string{"1.1.1.1"},
			},
			Linux: &types.CRILinuxPodSandboxConfig{
				Sysctls: map[string]string{"kernel.shm_rmid_forced": "1"},
			},
		})

		cfg, err := decodePodSandboxConfig(opts)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "my-pod", cfg.GetHostname())
		assert.Equal(t, []string{"1.1.1.1"}, cfg.GetDnsConfig().GetServers())
		assert.Equal(t, "1", cfg.GetLinux().GetSysctls()["kernel.shm_rmid_forced"])
	})

	t.Run("decodes repeated DNS fields and multiple sysctl entries", func(t *testing.T) {
		opts := marshalTestPodSandboxConfig(t, &types.CRIPodConfig{
			DnsConfig: &types.CRIDNSConfig{
				Servers:  []string{"1.1.1.1", "8.8.8.8"},
				Searches: []string{"svc.cluster.local", "cluster.local"},
				Options:  []string{"ndots:5"},
			},
			Linux: &types.CRILinuxPodSandboxConfig{
				Sysctls: map[string]string{
					"kernel.shm_rmid_forced": "1",
					"fs.mqueue.msg_max":      "100",
				},
			},
		})

		cfg, err := decodePodSandboxConfig(opts)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, []string{"1.1.1.1", "8.8.8.8"}, cfg.GetDnsConfig().GetServers())
		assert.Equal(t, []string{"svc.cluster.local", "cluster.local"}, cfg.GetDnsConfig().GetSearches())
		assert.Equal(t, []string{"ndots:5"}, cfg.GetDnsConfig().GetOptions())
		assert.Equal(t, map[string]string{
			"kernel.shm_rmid_forced": "1",
			"fs.mqueue.msg_max":      "100",
		}, cfg.GetLinux().GetSysctls())
	})

	t.Run("errors for a mismatched type URL", func(t *testing.T) {
		data, err := proto.Marshal(&types.CRIDNSConfig{Servers: []string{"1.1.1.1"}})
		require.NoError(t, err)
		opts := &anypb.Any{TypeUrl: "runtime.v1.DNSConfig", Value: data}

		_, err = decodePodSandboxConfig(opts)
		assert.Error(t, err)
	})

	t.Run("errors for the right type URL but unparsable bytes", func(t *testing.T) {
		opts := &anypb.Any{TypeUrl: podSandboxConfigTypeURL, Value: []byte("not a valid protobuf message")}

		_, err := decodePodSandboxConfig(opts)
		assert.Error(t, err)
	})
}

func TestAddHostname(t *testing.T) {
	t.Run("empty hostname is a no-op", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{}}
		require.NoError(t, addHostname(context.Background(), b, ""))
		assert.Empty(t, b.Spec.Hostname)
		assert.Empty(t, b.Spec.Mounts)
	})

	t.Run("sets spec.Hostname and adds an /etc/hostname mount", func(t *testing.T) {
		b := loadTestBundle(t, specs.Spec{})
		require.NoError(t, addHostname(context.Background(), b, "my-pod"))
		assert.Equal(t, "my-pod", b.Spec.Hostname)

		require.Len(t, b.Spec.Mounts, 1)
		assert.Equal(t, "/etc/hostname", b.Spec.Mounts[0].Destination)
		assert.Equal(t, "hostname", b.Spec.Mounts[0].Source)

		files, err := b.Files()
		require.NoError(t, err)
		assert.Equal(t, "my-pod\n", string(files["hostname"]))
	})

	t.Run("existing /etc/hostname mount is left untouched", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{Mounts: []specs.Mount{
			{Destination: "/etc/hostname", Type: "bind", Source: "/custom/hostname"},
		}}}
		require.NoError(t, addHostname(context.Background(), b, "my-pod"))
		// spec.Hostname is still set (it's a separate mechanism from the
		// file mount and crun applies it regardless of /etc/hostname).
		assert.Equal(t, "my-pod", b.Spec.Hostname)
		require.Len(t, b.Spec.Mounts, 1)
		assert.Equal(t, "/custom/hostname", b.Spec.Mounts[0].Source)
	})
}

func TestAddSysctls(t *testing.T) {
	t.Run("empty map is a no-op", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{}}
		require.NoError(t, addSysctls(context.Background(), b, nil))
		assert.Nil(t, b.Spec.Linux)
	})

	t.Run("merges into a nil Linux/Sysctl", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{}}
		require.NoError(t, addSysctls(context.Background(), b, map[string]string{
			"kernel.shm_rmid_forced": "1",
		}))
		require.NotNil(t, b.Spec.Linux)
		assert.Equal(t, "1", b.Spec.Linux.Sysctl["kernel.shm_rmid_forced"])
	})

	t.Run("existing per-container sysctl wins on collision", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{Linux: &specs.Linux{
			Sysctl: map[string]string{"kernel.shm_rmid_forced": "0"},
		}}}
		require.NoError(t, addSysctls(context.Background(), b, map[string]string{
			"kernel.shm_rmid_forced": "1",
			"fs.mqueue.msg_max":      "100",
		}))
		assert.Equal(t, "0", b.Spec.Linux.Sysctl["kernel.shm_rmid_forced"])
		assert.Equal(t, "100", b.Spec.Linux.Sysctl["fs.mqueue.msg_max"])
	})
}
