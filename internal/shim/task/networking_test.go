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
	"net"
	"net/netip"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	require.NoError(t, err)
	return mac
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	require.NoError(t, err)
	return p
}

func TestParseNetwork(t *testing.T) {
	testcases := []struct {
		name       string
		annotation string
		wantErr    string
		want       network
	}{
		{
			name:       "minimal valid: socket, mode, mac",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4",
			want: network{
				endpoint: "/tmp/net.sock",
				mode:     "unixgram",
				mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
			},
		},
		{
			name:       "unixstream mode",
			annotation: "socket=/tmp/net.sock,mode=unixstream,mac=fa:43:25:5d:6f:b4",
			want: network{
				endpoint: "/tmp/net.sock",
				mode:     "unixstream",
				mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
			},
		},
		{
			name:       "invalid mode",
			annotation: "socket=/tmp/net.sock,mode=tcp,mac=fa:43:25:5d:6f:b4",
			wantErr:    "invalid network mode: tcp",
		},
		{
			name:       "invalid mac",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=not-a-mac",
			wantErr:    "parsing MAC address",
		},
		{
			name:       "multicast mac rejected",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=ff:43:25:5d:6f:b4",
			wantErr:    "multicast bit is set",
		},
		{
			name:       "dhcp true",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,dhcp=true",
			want: network{
				endpoint: "/tmp/net.sock",
				mode:     "unixgram",
				mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
				dhcp:     true,
			},
		},
		{
			name:       "invalid dhcp value",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,dhcp=maybe",
			wantErr:    "parsing DHCP field",
		},
		{
			name:       "ipv4 address",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,addr=192.168.127.2/24",
			want: network{
				endpoint: "/tmp/net.sock",
				mode:     "unixgram",
				mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
				addr4:    mustPrefix(t, "192.168.127.2/24"),
			},
		},
		{
			name:       "ipv4 and ipv6 addresses",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,addr=192.168.127.2/24,addr=fd00::2/64",
			want: network{
				endpoint: "/tmp/net.sock",
				mode:     "unixgram",
				mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
				addr4:    mustPrefix(t, "192.168.127.2/24"),
				addr6:    mustPrefix(t, "fd00::2/64"),
			},
		},
		{
			name:       "duplicate ipv4 address rejected",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,addr=192.168.127.2/24,addr=192.168.127.3/24",
			wantErr:    "multiple IPv4 addresses specified",
		},
		{
			name:       "duplicate ipv6 address rejected",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,addr=fd00::2/64,addr=fd00::3/64",
			wantErr:    "multiple IPv6 addresses specified",
		},
		{
			name:       "invalid address",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,addr=not-an-addr",
			wantErr:    "parsing address",
		},
		{
			name:       "vfkit true",
			annotation: "socket=/tmp/net.sock,mode=unixstream,mac=fa:43:25:5d:6f:b4,vfkit=true",
			want: network{
				endpoint: "/tmp/net.sock",
				mode:     "unixstream",
				mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
				vfkit:    true,
			},
		},
		{
			name:       "invalid vfkit value",
			annotation: "socket=/tmp/net.sock,mode=unixstream,mac=fa:43:25:5d:6f:b4,vfkit=maybe",
			wantErr:    "parsing vfkit field",
		},
		{
			name:       "vnet_hdr true",
			annotation: "socket=/tmp/net.sock,mode=unixstream,mac=fa:43:25:5d:6f:b4,vnet_hdr=true",
			want: network{
				endpoint: "/tmp/net.sock",
				mode:     "unixstream",
				mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
				vnetHdr:  true,
			},
		},
		{
			name:       "invalid vnet_hdr value",
			annotation: "socket=/tmp/net.sock,mode=unixstream,mac=fa:43:25:5d:6f:b4,vnet_hdr=maybe",
			wantErr:    "parsing vnet_hdr field",
		},
		{
			name:       "features parsed",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,features=VIRTIO_NET_F_CSUM",
			want: network{
				endpoint: "/tmp/net.sock",
				mode:     "unixgram",
				mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
				features: 1 << 0,
			},
		},
		{
			name:       "invalid features value",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,features=NOT_A_FEATURE",
			wantErr:    "parsing features",
		},
		{
			name:       "unknown field rejected",
			annotation: "socket=/tmp/net.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4,bogus=1",
			wantErr:    "unknown network field: bogus",
		},
		{
			name:       "malformed field (no equals)",
			annotation: "socket=/tmp/net.sock,mode",
			wantErr:    "invalid network field: mode",
		},
		{
			name:       "missing socket",
			annotation: "mode=unixgram,mac=fa:43:25:5d:6f:b4",
			wantErr:    "missing required field(s)",
		},
		{
			name:       "missing mode",
			annotation: "socket=/tmp/net.sock,mac=fa:43:25:5d:6f:b4",
			wantErr:    "missing required field(s)",
		},
		{
			name:       "missing mac",
			annotation: "socket=/tmp/net.sock,mode=unixgram",
			wantErr:    "missing required field(s)",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNetwork(tc.annotation)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want.endpoint, got.endpoint)
			assert.Equal(t, tc.want.mode, got.mode)
			assert.Equal(t, tc.want.mac, got.mac)
			assert.Equal(t, tc.want.dhcp, got.dhcp)
			assert.Equal(t, tc.want.addr4, got.addr4)
			assert.Equal(t, tc.want.addr6, got.addr6)
			assert.Equal(t, tc.want.features, got.features)
			assert.Equal(t, tc.want.vfkit, got.vfkit)
			assert.Equal(t, tc.want.vnetHdr, got.vnetHdr)
		})
	}
}

// TestNetworksProviderFromBundle verifies that FromBundle parses every
// io.containerd.nerdbox.network.* annotation, strips them from the spec sent
// to the guest, and reports parse errors from malformed annotations.
func TestNetworksProviderFromBundle(t *testing.T) {
	t.Run("no annotations is a no-op", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{}}
		var p networksProvider
		require.NoError(t, p.FromBundle(context.Background(), b))
		assert.Empty(t, p.nws)
	})

	t.Run("parses multiple network annotations and strips them", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{Annotations: map[string]string{
			"io.containerd.nerdbox.network.0": "socket=/tmp/a.sock,mode=unixgram,mac=fa:43:25:5d:6f:b4",
			"io.containerd.nerdbox.other":     "unrelated",
		}}}
		var p networksProvider
		require.NoError(t, p.FromBundle(context.Background(), b))
		require.Len(t, p.nws, 1)
		assert.Equal(t, "/tmp/a.sock", p.nws[0].endpoint)
		// Only the matching annotation is stripped.
		_, hasNetworkAnnot := b.Spec.Annotations["io.containerd.nerdbox.network.0"]
		assert.False(t, hasNetworkAnnot)
		_, hasOther := b.Spec.Annotations["io.containerd.nerdbox.other"]
		assert.True(t, hasOther)
	})

	t.Run("propagates parse errors", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{Annotations: map[string]string{
			"io.containerd.nerdbox.network.0": "mode=bogus",
		}}}
		var p networksProvider
		err := p.FromBundle(context.Background(), b)
		require.Error(t, err)
		assert.ErrorContains(t, err, "failed to parse network annotation")
	})
}

// TestNetworksProviderSandboxOptions verifies that SandboxOptions translates
// parsed network annotations into the correct sandbox.WithNIC options,
// including mode selection and flag composition.
func TestNetworksProviderSandboxOptions(t *testing.T) {
	p := networksProvider{nws: []network{
		{
			endpoint: "/tmp/a.sock",
			mode:     "unixstream",
			mac:      mustMAC(t, "fa:43:25:5d:6f:b4"),
			vfkit:    true,
			vnetHdr:  true,
		},
		{
			endpoint: "/tmp/b.sock",
			mode:     "unixgram",
			mac:      mustMAC(t, "fa:43:25:5d:6f:b5"),
		},
	}}

	opts := p.SandboxOptions()
	require.Len(t, opts, 2)

	var o sandbox.Options
	for _, opt := range opts {
		opt(&o)
	}

	require.Len(t, o.NICs, 2)
	assert.Equal(t, "/tmp/a.sock", o.NICs[0].Endpoint)
	assert.Equal(t, uint32(NET_FLAG_VFKIT|NET_FLAG_INCLUDE_VNET_HEADER), o.NICs[0].Flags)
	assert.Equal(t, "/tmp/b.sock", o.NICs[1].Endpoint)
	assert.Equal(t, uint32(0), o.NICs[1].Flags)
}

// TestNetworksProviderInitArgs verifies that InitArgs formats the guest
// network configuration string with the expected fields and ordering.
func TestNetworksProviderInitArgs(t *testing.T) {
	p := networksProvider{nws: []network{
		{
			mac:   mustMAC(t, "fa:43:25:5d:6f:b4"),
			dhcp:  true,
			addr4: mustPrefix(t, "192.168.127.2/24"),
		},
		{
			mac: mustMAC(t, "fa:43:25:5d:6f:b5"),
		},
	}}

	args := p.InitArgs()
	require.Len(t, args, 2)
	assert.Equal(t, "-network=mac=fa:43:25:5d:6f:b4,dhcp=true,addr=192.168.127.2/24", args[0])
	assert.Equal(t, "-network=mac=fa:43:25:5d:6f:b5", args[1])
}
