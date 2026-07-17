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
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/nerdbox/internal/nwcfg"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// ifNameSize is the maximum length of a network interface name (including NUL terminator).
// This matches IFNAMSIZ from <net/if.h> on Linux.
const ifNameSize = 16

// ctrNetConfig is used to assemble network configuration for the VM.
// Its JSON serialization is passed to the VM along with the bundle.
type ctrNetConfig nwcfg.Config

const (
	// ctrDNSAnnotation is a CSV-encoded OCI annotation that describes the content
	// of a container's /etc/resolv.conf. Each key=value field is added directly
	// to the file (without the '='), separated by newlines.
	ctrDNSAnnotation = "io.containerd.nerdbox.ctr.dns"

	// ctrNetworkAnnotation is a CSV-encoded OCI annotation that specifies how
	// networking is configured for a container.
	ctrNetworkAnnotation = "io.containerd.nerdbox.ctr.network"
	// CSV fields that can be used in ctrNetworkAnnotation's value:
	vmMACField   = "vmmac"
	ctrMACField  = "mac"
	ctrAddrField = "addr"
	ctrIfName    = "ifname"
	ctrGateway   = "gw"
)

// fromBundle configures the networksProvider based on OCI annotations found in
// the bundle spec.
func (p *ctrNetConfig) fromBundle(ctx context.Context, b *bundle.Bundle) error {
	if b.Spec.Annotations == nil {
		return nil
	}

	for annotKey, annotValue := range b.Spec.Annotations {
		if !strings.HasPrefix(annotKey, ctrNetworkAnnotation+".") {
			continue
		}
		// The VM gets the parsed result, it doesn't need the annotation.
		delete(b.Spec.Annotations, annotKey)

		nw, err := parseCtrNetwork(annotValue)
		if err != nil {
			return fmt.Errorf("failed to parse container network annotation: %w", err)
		}
		p.Networks = append(p.Networks, nw)
	}

	return nil
}

func parseCtrNetwork(annotation string) (nwcfg.Network, error) {
	var n nwcfg.Network

	for _, field := range strings.Split(annotation, ",") {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			return nwcfg.Network{}, fmt.Errorf("invalid container network field: %s", field)
		}

		key := parts[0]
		value := parts[1]

		switch key {
		case vmMACField:
			if n.VmMAC != "" {
				return nwcfg.Network{}, fmt.Errorf("multiple VM MAC addresses specified")
			}
			mac, err := net.ParseMAC(value)
			if err != nil {
				return nwcfg.Network{}, fmt.Errorf("parsing MAC address: %w", err)
			}
			if (mac[0] & 0x1) != 0 {
				return nwcfg.Network{}, fmt.Errorf("invalid VM MAC address %s: multicast bit is set", value)
			}
			n.VmMAC = mac.String()
		case ctrMACField:
			if n.MAC != "" {
				return nwcfg.Network{}, fmt.Errorf("multiple container MAC addresses specified")
			}
			mac, err := net.ParseMAC(value)
			if err != nil {
				return nwcfg.Network{}, fmt.Errorf("parsing container MAC address: %w", err)
			}
			if (mac[0] & 0x1) != 0 {
				return nwcfg.Network{}, fmt.Errorf("invalid container MAC address %s: multicast bit is set", value)
			}
			n.MAC = mac.String()
		case ctrAddrField:
			addr, err := netip.ParsePrefix(value)
			if err != nil {
				return nwcfg.Network{}, fmt.Errorf("parsing container address: %w", err)
			}
			n.Addrs = append(n.Addrs, addr)
		case ctrGateway:
			addr, err := netip.ParseAddr(value)
			if err != nil {
				return nwcfg.Network{}, fmt.Errorf("parsing gateway address: %w", err)
			}
			if addr.Is4() {
				if n.DefaultGw4.IsValid() {
					return nwcfg.Network{}, fmt.Errorf("multiple IPv4 gateways specified")
				}
				n.DefaultGw4 = addr
			} else {
				if n.DefaultGw6.IsValid() {
					return nwcfg.Network{}, fmt.Errorf("multiple IPv6 gateways specified")
				}
				n.DefaultGw6 = addr
			}
		case ctrIfName:
			if len(value) >= ifNameSize {
				return nwcfg.Network{}, fmt.Errorf("interface name has more than %d characters: %s",
					ifNameSize-1, value[:ifNameSize-1]+"...")
			}
			n.IfName = value
		default:
			return nwcfg.Network{}, fmt.Errorf("unknown network field: %s", key)
		}
	}

	// n.VmMAC is required as it is used to identify the network.
	if n.VmMAC == "" {
		return nwcfg.Network{}, fmt.Errorf("'vmmac' is missing")
	}

	return n, nil
}

// addResolvConf adds a /etc/resolv.conf to the container, unless the
// bundle already includes one.
func addResolvConf(ctx context.Context, b *bundle.Bundle, fallbackToHostRC bool) error {
	// If there's already a resolv.conf mount, don't do anything.
	if slices.ContainsFunc(b.Spec.Mounts, func(m specs.Mount) bool {
		return m.Destination == "/etc/resolv.conf"
	}) {
		return nil
	}

	var rcBytes []byte
	if rcCSV, ok := b.Spec.Annotations[ctrDNSAnnotation]; ok {
		// Generate a resolv.conf file based on the annotation.
		// The VM gets the resolv.conf file, it doesn't need the annotation.
		delete(b.Spec.Annotations, ctrDNSAnnotation)

		rcBuf := bytes.Buffer{}
		rcBuf.Grow(len(rcCSV) + 64) // Should be enough space.
		for _, field := range strings.Split(rcCSV, ",") {
			k, v, found := strings.Cut(field, "=")
			_, _ = rcBuf.WriteString(k)
			if found {
				_, _ = rcBuf.WriteRune(' ')
				_, _ = rcBuf.WriteString(v)
			}
			_, _ = rcBuf.WriteRune('\n')
		}
		rcBytes = rcBuf.Bytes()
	} else if fallbackToHostRC {
		// Try giving the VM a copy of the host's resolv.conf.
		if c, err := os.ReadFile(hostResolvConfPath()); err == nil {
			rcBytes = c
		}
	}

	// Default to the VM's /etc/resolv.conf when there's no explicit config.
	source := "/etc/resolv.conf"
	if len(rcBytes) > 0 {
		b.AddExtraFile("resolv.conf", rcBytes)
		source = "resolv.conf"
	}

	b.Spec.Mounts = append(b.Spec.Mounts, specs.Mount{
		Destination: "/etc/resolv.conf",
		Type:        "bind",
		Source:      source,
		Options:     []string{"rbind", "rprivate"},
	})
	return nil
}

// systemdResolvedFullRC is the "full" resolv.conf systemd-resolved maintains
// alongside its stub file, listing the actual upstream DNS servers rather
// than the stub's loopback listener. See resolv.conf(5) /
// systemd-resolved.service(8).
const systemdResolvedFullRC = "/run/systemd/resolve/resolv.conf"

// hostResolvConfPath returns the host-side resolv.conf path to copy into a
// container's guest environment.
//
// Many Linux distributions symlink /etc/resolv.conf to systemd-resolved's
// stub file, whose sole nameserver is a loopback address (127.0.0.53) that
// only systemd-resolved's own stub listener answers on the host. Copying
// that verbatim into an isolated environment (a container network
// namespace, or — as here — a VM) is not useful: nothing answers on that
// loopback address there, so DNS queries fail even though the host itself
// resolves names correctly. Docker and containerd's CRI implementation
// handle this exact case the same way: prefer the "full" resolv.conf
// systemd-resolved also maintains, which lists the real upstream
// nameservers, over the stub file. This is not specific to nerdbox or to
// any particular guest network mechanism — it is a general consequence of
// copying host DNS configuration into an isolated environment.
func hostResolvConfPath() string {
	const hostRC = "/etc/resolv.conf"
	data, err := os.ReadFile(hostRC)
	if err != nil || !onlyLoopbackNameservers(data) {
		return hostRC
	}
	if _, err := os.Stat(systemdResolvedFullRC); err == nil {
		return systemdResolvedFullRC
	}
	return hostRC
}

// onlyLoopbackNameservers reports whether every "nameserver" line in a
// resolv.conf file's contents resolves to a loopback address. Returns false
// if there are no nameserver lines at all (nothing to prefer an alternative
// over).
func onlyLoopbackNameservers(resolvConf []byte) bool {
	found := false
	for _, line := range strings.Split(string(resolvConf), "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(fields[1])
		if ip == nil || !ip.IsLoopback() {
			return false
		}
		found = true
	}
	return found
}
