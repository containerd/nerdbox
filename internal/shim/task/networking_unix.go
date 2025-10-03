package task

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/virtionet"
	"github.com/containerd/nerdbox/internal/vm"
)

type networksProvider struct {
	nws []network
}

type network struct {
	endpoint string           // endpoint is the path to the UNIX socket serving that network endpoint
	mode     string           // mode is either "unixgram" or "unixstream"
	mac      net.HardwareAddr // mac is the MAC address of the network interface
	addr4    netip.Prefix     // addr4 is the IPv4 address + subnet mask of the network interface
	addr6    netip.Prefix     // addr6 is the IPv6 address + subnet mask of the network interface
	features uint32           // features is a bitmask of virtio-net features enabled on this network endpoint
	vfkit    bool             // vfkit is a boolean flag indicating whether libkrun must send the VFKIT magic sequence after connecting to the socket.
}

const (
	// networkAnnotation is a CSV-encoded OCI annotation that specifies how
	// networking is configured for a VM.
	networkAnnotation = "io.containerd.nerdbox.network"

	socketField   = "socket"
	modeField     = "mode"
	macField      = "mac"
	addrField     = "addr"
	featuresField = "features" // features is a bitwise-OR separated list of virtio-net features. See https://docs.oasis-open.org/virtio/virtio/v1.3/csd01/virtio-v1.3-csd01.html#x1-2370003
	vfkitField    = "vfkit"    // vfkit is a boolean flag indicating whether libkrun must send the VFKIT magic sequence after connecting to the socket.

	nwModeUnixgram   = "unixgram"
	nwModeUnixstream = "unixstream"
)

// FromBundle configures the networksProvider based on OCI annotations found in
// the bundle spec.
func (p *networksProvider) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	if b.Spec.Annotations == nil {
		return nil
	}

	for annotKey, annotValue := range b.Spec.Annotations {
		if !strings.HasPrefix(annotKey, networkAnnotation+".") {
			continue
		}
		// We want to keep the OCI spec sent to the VM clean of any host-specific
		// annotations.
		delete(b.Spec.Annotations, annotKey)

		nw, err := parseNetwork(annotValue)
		if err != nil {
			return fmt.Errorf("failed to parse network annotation: %w", err)
		}
		p.nws = append(p.nws, nw)
	}

	return nil
}

func parseNetwork(annotation string) (network, error) {
	var n network

	for _, field := range strings.Split(annotation, ",") {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			return network{}, fmt.Errorf("invalid network field: %s", field)
		}

		key := parts[0]
		value := parts[1]

		switch key {
		case socketField:
			n.endpoint = value
		case modeField:
			if value != nwModeUnixgram && value != nwModeUnixstream {
				return network{}, fmt.Errorf("invalid network mode: %s", value)
			}
			n.mode = value
		case macField:
			var err error
			n.mac, err = net.ParseMAC(value)
			if err != nil {
				return network{}, fmt.Errorf("parsing MAC address: %w", err)
			}
			if (n.mac[0] & 0xfe) != n.mac[0] {
				return network{}, errors.New("invalid MAC address: multicast bit is set")
			}
		case addrField:
			addr, err := netip.ParsePrefix(value)
			if err != nil {
				return network{}, fmt.Errorf("parsing address: %w", err)
			}
			if addr.Addr().Is4() {
				if n.addr4.IsValid() {
					return network{}, fmt.Errorf("multiple IPv4 addresses specified")
				}
				n.addr4 = addr
			} else {
				if n.addr6.IsValid() {
					return network{}, fmt.Errorf("multiple IPv6 addresses specified")
				}
				n.addr6 = addr
			}
		case featuresField:
			var err error
			if n.features, err = parseVirtioNetFeatures(value); err != nil {
				return network{}, fmt.Errorf("parsing features: %w", err)
			}
		case vfkitField:
			vfkit, err := strconv.ParseBool(value)
			if err != nil {
				return network{}, fmt.Errorf("parsing vfkit field: %w", err)
			}
			n.vfkit = vfkit
		default:
			return network{}, fmt.Errorf("unknown network field: %s", key)
		}
	}

	if n.endpoint == "" || n.mode == "" || n.mac == nil || (!n.addr4.IsValid() && !n.addr6.IsValid()) {
		return network{}, fmt.Errorf("either 'endpoint', 'mode', 'mac' or 'addr' is missing")
	}

	return n, nil
}

func parseVirtioNetFeatures(value string) (uint32, error) {
	f, err := virtionet.FeaturesFromStrings(strings.Split(value, "|")...)
	if err != nil {
		return 0, err
	}
	return f.AsUint32(), nil
}

// SetupVM configures the VM to use the network provider set up through OCI
// annotations (if any).
func (p *networksProvider) SetupVM(ctx context.Context, vmi vm.Instance) error {
	for _, nw := range p.nws {
		nwMode := vm.NetworkModeUnixgram
		if nw.mode == "unixstream" {
			nwMode = vm.NetworkModeUnixstream
		}

		var flags uint32
		if nw.vfkit {
			flags = 1 // See https://github.com/containers/libkrun/blob/357ec63fee444b973e4fc76d2121fd41631f121e/include/libkrun.h#L271C9-L271C23
		}

		if err := vmi.AddNIC(ctx, nw.endpoint, nw.mac, nwMode, nw.features, flags); err != nil {
			return err
		}
	}
	return nil
}

// InitArgs returns the arguments for the init process to set up networking
// within the VM.
func (p *networksProvider) InitArgs() []string {
	args := make([]string, 0, len(p.nws))
	for _, nw := range p.nws {
		if nw.addr4.IsValid() {
			args = append(args, fmt.Sprintf("-network=mac=%s,addr=%s", nw.mac, nw.addr4))
		}
		if nw.addr6.IsValid() {
			args = append(args, fmt.Sprintf("-network=mac=%s,addr=%s", nw.mac, nw.addr6))
		}
	}
	return args
}
