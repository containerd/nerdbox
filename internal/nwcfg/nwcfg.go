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

// Package nwcfg describes the network config file passed to the VM along with the bundle.
package nwcfg

import "net/netip"

// Filename is the name of the JSON file passed to the VM in the bundle directory.
const Filename = "nw-config.json"

// Config describes the network configuration passed to the VM. When marshalled to
// JSON, it can be passed to the VM via the bundle directory.
type Config struct {
	Networks []Network
}

// Network describes a single network interface for the container.
// The network is identified by VmMAC, the MAC address of the VM's network interface.
type Network struct {
	VmMAC      string         // VmMAC is the MAC address of the VM's network interface (required)
	MAC        string         `json:",omitempty"` // MAC is the MAC address of the container's network interface
	Addrs      []netip.Prefix `json:",omitempty"` // Addrs are addresses (with subnet masks) for the container's interface
	IfName     string         `json:",omitempty"` // IfName is the name of the container's network interface
	DefaultGw4 netip.Addr     `json:",omitzero"`  // DefaultGw4 is the IPv4 default gateway for the container
	DefaultGw6 netip.Addr     `json:",omitzero"`  // DefaultGw6 is the IPv6 default gateway for the container
}
