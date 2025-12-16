//go:build linux

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
		fields := []string{"mac=" + nw.MAC.String()}
		if nw.DHCP {
			fields = append(fields, "dhcp=true")
		}
		if nw.Addr4.IsValid() {
			fields = append(fields, "addr="+nw.Addr4.String())
		}
		if nw.Addr6.IsValid() {
			fields = append(fields, "addr="+nw.Addr6.String())
		}
		ss = append(ss, strings.Join(fields, ","))
	}
	return strings.Join(ss, " ")
}

func (n *networks) Set(value string) error {
	var nw vmnetworking.Network
	for _, kv := range strings.Split(value, ",") {
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
			if addr.Addr().Is4() {
				nw.Addr4 = addr
			} else {
				nw.Addr6 = addr
			}
		case "dhcp":
			dhcp, err := strconv.ParseBool(parts[1])
			if err != nil {
				return fmt.Errorf("invalid DHCP field: %w", err)
			}
			nw.DHCP = dhcp
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
