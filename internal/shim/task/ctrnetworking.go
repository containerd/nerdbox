package task

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/containerd/nerdbox/internal/nwcfg"
	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// ctrNetConfig is used to assemble network configuration for the VM.
// Its JSON serialization is passed to the VM along with the bundle.
type ctrNetConfig nwcfg.Config

const (
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
			if len(value) >= unix.IFNAMSIZ {
				return nwcfg.Network{}, fmt.Errorf("interface name has more than %d characters: %s",
					unix.IFNAMSIZ-1, value[:unix.IFNAMSIZ-1]+"...")
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
