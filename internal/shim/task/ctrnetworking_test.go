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
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/containerd/nerdbox/internal/nwcfg"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	require.NoError(t, err)
	return a
}

// loadTestBundle writes spec to a temporary bundle directory and loads it
// via bundle.Load, so that b.AddExtraFile/b.Files() work correctly (the
// extraFiles map is only initialized by Load, not by a bare struct literal).
func loadTestBundle(t *testing.T, spec specs.Spec) *bundle.Bundle {
	t.Helper()
	if spec.Root == nil {
		spec.Root = &specs.Root{Path: "rootfs"}
	}
	dir := t.TempDir()
	data, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644))
	b, err := bundle.Load(context.Background(), dir)
	require.NoError(t, err)
	return b
}

func TestParseCtrNetwork(t *testing.T) {
	testcases := []struct {
		name       string
		annotation string
		wantErr    string
		want       nwcfg.Network
	}{
		{
			name:       "minimal valid: vmmac only",
			annotation: "vmmac=fa:43:25:5d:6f:b4",
			want:       nwcfg.Network{VmMAC: "fa:43:25:5d:6f:b4"},
		},
		{
			name:       "missing vmmac",
			annotation: "mac=fa:43:25:5d:6f:b5",
			wantErr:    "'vmmac' is missing",
		},
		{
			name:       "invalid vmmac",
			annotation: "vmmac=not-a-mac",
			wantErr:    "parsing MAC address",
		},
		{
			name:       "multicast vmmac rejected",
			annotation: "vmmac=ff:43:25:5d:6f:b4",
			wantErr:    "multicast bit is set",
		},
		{
			name:       "duplicate vmmac rejected",
			annotation: "vmmac=fa:43:25:5d:6f:b4,vmmac=fa:43:25:5d:6f:b5",
			wantErr:    "multiple VM MAC addresses specified",
		},
		{
			name:       "container mac",
			annotation: "vmmac=fa:43:25:5d:6f:b4,mac=fa:43:25:5d:6f:b5",
			want: nwcfg.Network{
				VmMAC: "fa:43:25:5d:6f:b4",
				MAC:   "fa:43:25:5d:6f:b5",
			},
		},
		{
			name:       "invalid container mac",
			annotation: "vmmac=fa:43:25:5d:6f:b4,mac=not-a-mac",
			wantErr:    "parsing container MAC address",
		},
		{
			name:       "multicast container mac rejected",
			annotation: "vmmac=fa:43:25:5d:6f:b4,mac=ff:43:25:5d:6f:b4",
			wantErr:    "multicast bit is set",
		},
		{
			name:       "duplicate container mac rejected",
			annotation: "vmmac=fa:43:25:5d:6f:b4,mac=fa:43:25:5d:6f:b5,mac=fa:43:25:5d:6f:b6",
			wantErr:    "multiple container MAC addresses specified",
		},
		{
			name:       "container address",
			annotation: "vmmac=fa:43:25:5d:6f:b4,addr=192.168.1.2/24",
			want: nwcfg.Network{
				VmMAC: "fa:43:25:5d:6f:b4",
				Addrs: []netip.Prefix{mustPrefix(t, "192.168.1.2/24")},
			},
		},
		{
			name:       "invalid container address",
			annotation: "vmmac=fa:43:25:5d:6f:b4,addr=not-an-addr",
			wantErr:    "parsing container address",
		},
		{
			name:       "ipv4 gateway",
			annotation: "vmmac=fa:43:25:5d:6f:b4,gw=192.168.1.1",
			want: nwcfg.Network{
				VmMAC:      "fa:43:25:5d:6f:b4",
				DefaultGw4: mustAddr(t, "192.168.1.1"),
			},
		},
		{
			name:       "ipv6 gateway",
			annotation: "vmmac=fa:43:25:5d:6f:b4,gw=fd00::1",
			want: nwcfg.Network{
				VmMAC:      "fa:43:25:5d:6f:b4",
				DefaultGw6: mustAddr(t, "fd00::1"),
			},
		},
		{
			name:       "invalid gateway",
			annotation: "vmmac=fa:43:25:5d:6f:b4,gw=not-an-addr",
			wantErr:    "parsing gateway address",
		},
		{
			name:       "duplicate ipv4 gateway rejected",
			annotation: "vmmac=fa:43:25:5d:6f:b4,gw=192.168.1.1,gw=192.168.1.2",
			wantErr:    "multiple IPv4 gateways specified",
		},
		{
			name:       "duplicate ipv6 gateway rejected",
			annotation: "vmmac=fa:43:25:5d:6f:b4,gw=fd00::1,gw=fd00::2",
			wantErr:    "multiple IPv6 gateways specified",
		},
		{
			name:       "interface name",
			annotation: "vmmac=fa:43:25:5d:6f:b4,ifname=eth1",
			want: nwcfg.Network{
				VmMAC:  "fa:43:25:5d:6f:b4",
				IfName: "eth1",
			},
		},
		{
			name:       "interface name too long",
			annotation: "vmmac=fa:43:25:5d:6f:b4,ifname=" + strings.Repeat("x", ifNameSize),
			wantErr:    "interface name has more than",
		},
		{
			name:       "unknown field rejected",
			annotation: "vmmac=fa:43:25:5d:6f:b4,bogus=1",
			wantErr:    "unknown network field: bogus",
		},
		{
			name:       "malformed field (no equals)",
			annotation: "vmmac",
			wantErr:    "invalid container network field: vmmac",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCtrNetwork(tc.annotation)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCtrNetConfigFromBundle verifies that fromBundle parses every
// io.containerd.nerdbox.ctr.network.* annotation, strips them from the spec
// sent to the guest, and reports parse errors from malformed annotations.
func TestCtrNetConfigFromBundle(t *testing.T) {
	t.Run("no annotations is a no-op", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{}}
		var c ctrNetConfig
		require.NoError(t, c.fromBundle(context.Background(), b))
		assert.Empty(t, c.Networks)
	})

	t.Run("parses annotation and strips it", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{Annotations: map[string]string{
			"io.containerd.nerdbox.ctr.network.0": "vmmac=fa:43:25:5d:6f:b4",
			"io.containerd.nerdbox.other":         "unrelated",
		}}}
		var c ctrNetConfig
		require.NoError(t, c.fromBundle(context.Background(), b))
		require.Len(t, c.Networks, 1)
		assert.Equal(t, "fa:43:25:5d:6f:b4", c.Networks[0].VmMAC)
		_, hasNetworkAnnot := b.Spec.Annotations["io.containerd.nerdbox.ctr.network.0"]
		assert.False(t, hasNetworkAnnot)
		_, hasOther := b.Spec.Annotations["io.containerd.nerdbox.other"]
		assert.True(t, hasOther)
	})

	t.Run("propagates parse errors", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{Annotations: map[string]string{
			"io.containerd.nerdbox.ctr.network.0": "mac=not-a-mac",
		}}}
		var c ctrNetConfig
		err := c.fromBundle(context.Background(), b)
		require.Error(t, err)
		assert.ErrorContains(t, err, "failed to parse container network annotation")
	})
}

// TestAddResolvConf covers the three resolv.conf sources handled by
// addResolvConf: an explicit DNS annotation, a fallback to the host's
// /etc/resolv.conf, and skipping when the bundle already mounts one.
func TestAddResolvConf(t *testing.T) {
	t.Run("existing resolv.conf mount is left untouched", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{Mounts: []specs.Mount{
			{Destination: "/etc/resolv.conf", Type: "bind", Source: "/custom/resolv.conf"},
		}}}
		require.NoError(t, addResolvConf(context.Background(), b, true, nil))
		require.Len(t, b.Spec.Mounts, 1)
		assert.Equal(t, "/custom/resolv.conf", b.Spec.Mounts[0].Source)
	})

	t.Run("dns annotation generates resolv.conf content", func(t *testing.T) {
		b := loadTestBundle(t, specs.Spec{Annotations: map[string]string{
			"io.containerd.nerdbox.ctr.dns": "nameserver=8.8.8.8,search=example.com",
		}})
		require.NoError(t, addResolvConf(context.Background(), b, false, nil))

		// Annotation is stripped after being consumed.
		_, hasAnnot := b.Spec.Annotations["io.containerd.nerdbox.ctr.dns"]
		assert.False(t, hasAnnot)

		require.Len(t, b.Spec.Mounts, 1)
		assert.Equal(t, "/etc/resolv.conf", b.Spec.Mounts[0].Destination)
		assert.Equal(t, "resolv.conf", b.Spec.Mounts[0].Source)

		files, err := b.Files()
		require.NoError(t, err)
		content := string(files["resolv.conf"])
		assert.Contains(t, content, "nameserver 8.8.8.8\n")
		assert.Contains(t, content, "search example.com\n")
	})

	t.Run("fallback to host resolv.conf when no annotation and fallback enabled", func(t *testing.T) {
		// addResolvConf copies a resolv.conf from the host when fallback is enabled
		// (preferring systemd-resolved's "full" file when /etc/resolv.conf is a
		// loopback stub). Regardless of host state, the mount destination is always
		// /etc/resolv.conf; the source is "resolv.conf" (extra file) when the host
		// file was read, or the VM's own /etc/resolv.conf when it was not.
		b := loadTestBundle(t, specs.Spec{})
		require.NoError(t, addResolvConf(context.Background(), b, true, nil))
		require.Len(t, b.Spec.Mounts, 1)
		assert.Equal(t, "/etc/resolv.conf", b.Spec.Mounts[0].Destination)
		assert.Contains(t, []string{"resolv.conf", "/etc/resolv.conf"}, b.Spec.Mounts[0].Source)
	})

	t.Run("no annotation and fallback disabled defaults to VM resolv.conf", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{}}
		require.NoError(t, addResolvConf(context.Background(), b, false, nil))
		require.Len(t, b.Spec.Mounts, 1)
		assert.Equal(t, "/etc/resolv.conf", b.Spec.Mounts[0].Destination)
		assert.Equal(t, "/etc/resolv.conf", b.Spec.Mounts[0].Source)
	})

	t.Run("pod DNSConfig generates resolv.conf content", func(t *testing.T) {
		b := loadTestBundle(t, specs.Spec{})
		podDNS := &criapi.DNSConfig{
			Servers:  []string{"1.1.1.1", "8.8.8.8"},
			Searches: []string{"svc.cluster.local", "cluster.local"},
			Options:  []string{"ndots:5"},
		}
		require.NoError(t, addResolvConf(context.Background(), b, false, podDNS))

		require.Len(t, b.Spec.Mounts, 1)
		assert.Equal(t, "/etc/resolv.conf", b.Spec.Mounts[0].Destination)
		assert.Equal(t, "resolv.conf", b.Spec.Mounts[0].Source)

		files, err := b.Files()
		require.NoError(t, err)
		content := string(files["resolv.conf"])
		assert.Contains(t, content, "nameserver 1.1.1.1\n")
		assert.Contains(t, content, "nameserver 8.8.8.8\n")
		assert.Contains(t, content, "search svc.cluster.local cluster.local\n")
		assert.Contains(t, content, "options ndots:5\n")
	})

	t.Run("dns annotation takes priority over pod DNSConfig", func(t *testing.T) {
		b := loadTestBundle(t, specs.Spec{Annotations: map[string]string{
			"io.containerd.nerdbox.ctr.dns": "nameserver=8.8.8.8",
		}})
		podDNS := &criapi.DNSConfig{Servers: []string{"1.1.1.1"}}
		require.NoError(t, addResolvConf(context.Background(), b, false, podDNS))

		files, err := b.Files()
		require.NoError(t, err)
		content := string(files["resolv.conf"])
		assert.Contains(t, content, "nameserver 8.8.8.8\n")
		assert.NotContains(t, content, "1.1.1.1")
	})

	t.Run("empty pod DNSConfig falls through to fallback", func(t *testing.T) {
		b := loadTestBundle(t, specs.Spec{})
		require.NoError(t, addResolvConf(context.Background(), b, false, &criapi.DNSConfig{}))
		require.Len(t, b.Spec.Mounts, 1)
		assert.Equal(t, "/etc/resolv.conf", b.Spec.Mounts[0].Source)
	})
}

// TestOnlyLoopbackNameservers covers the resolv.conf parsing used to detect
// a systemd-resolved-style stub resolver (a nameserver line pointing at a
// loopback address) versus a resolv.conf listing real, reachable
// nameservers.
func TestOnlyLoopbackNameservers(t *testing.T) {
	testcases := []struct {
		name string
		rc   string
		want bool
	}{
		{
			name: "single loopback nameserver (systemd-resolved stub)",
			rc:   "nameserver 127.0.0.53\noptions edns0 trust-ad\n",
			want: true,
		},
		{
			name: "single real nameserver",
			rc:   "nameserver 8.8.8.8\n",
			want: false,
		},
		{
			name: "mixed loopback and real nameservers is not all-loopback",
			rc:   "nameserver 127.0.0.53\nnameserver 1.1.1.1\n",
			want: false,
		},
		{
			name: "multiple loopback nameservers",
			rc:   "nameserver 127.0.0.53\nnameserver 127.0.0.54\n",
			want: true,
		},
		{
			name: "IPv6 loopback nameserver",
			rc:   "nameserver ::1\n",
			want: true,
		},
		{
			name: "no nameserver lines at all",
			rc:   "search example.com\noptions edns0\n",
			want: false,
		},
		{
			name: "empty file",
			rc:   "",
			want: false,
		},
		{
			name: "malformed nameserver line ignored",
			rc:   "nameserver\n",
			want: false,
		},
		{
			name: "comments and blank lines ignored",
			rc:   "# this is a comment\n\nnameserver 127.0.0.53\n",
			want: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got := onlyLoopbackNameservers([]byte(tc.rc))
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestHostResolvConfPath verifies that hostResolvConfPath prefers
// systemd-resolved's "full" resolv.conf when /etc/resolv.conf looks like the
// loopback stub file and the full file exists, and falls back to
// /etc/resolv.conf in every other case.
func TestHostResolvConfPath(t *testing.T) {
	// hostResolvConfPath reads fixed OS paths ("/etc/resolv.conf" and
	// systemdResolvedFullRC), so we can only exercise the "not a stub" or
	// "full file absent" fallback branches deterministically without
	// mocking the filesystem. What we can assert unconditionally: the
	// function always returns a non-empty path, and it only ever returns
	// one of the two known candidates.
	got := hostResolvConfPath()
	assert.Contains(t, []string{"/etc/resolv.conf", systemdResolvedFullRC}, got)
}
