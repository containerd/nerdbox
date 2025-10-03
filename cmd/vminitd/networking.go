//go:build linux

package main

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/containerd/nerdbox/internal/vminit/vmnetworking"
)

type networks []vmnetworking.Network

func (n *networks) String() string {
	ss := make([]string, 0, len(*n))
	for _, nw := range *n {
		ss = append(ss, fmt.Sprintf("mac=%s,addr=%s", nw.mac, nw.addr))
	}
	return strings.Join(ss, " ")
}

func (n *networks) Set(value string) error {
	kvs := strings.Split(value, ",")
	if len(kvs) != 2 {
		return fmt.Errorf("invalid network %q: expected format: mac=<mac>,[addr=<addr>|dhcp=<dhcp>]", value)
	}

	var nw vmnetworking.Network
	for _, kv := range kvs {
		parts := strings.Split(kv, "=")
		if len(parts) != 2 {
			return fmt.Errorf("invalid network %q: expected format: mac=<mac>,[addr=<addr>|dhcp=<dhcp>]", value)
		}
		switch parts[0] {
		case "mac":
			mac, err := net.ParseMAC(parts[1])
			if err != nil {
				return fmt.Errorf("invalid MAC address: %w", err)
			}
			nw.MAC = mac
		case "addr":
			addr, err := netip.ParsePrefix(parts[1])
			if err != nil {
				return fmt.Errorf("invalid IP address: %w", err)
			}
			nw.Addr = addr
		default:
			return fmt.Errorf("invalid network %q: unknown field %q", value, parts[0])
		}
	}

	if err := nw.Validate(); err != nil {
		return fmt.Errorf("invalid network %q: %w", value, err)
	}

	*n = append(*n, nw)
	return nil
}
