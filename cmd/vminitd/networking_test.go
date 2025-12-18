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
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseNetwork(t *testing.T) {
	testscases := []struct {
		name  string
		input string
		want  networks
	}{
		{
			name:  "ipv4 only",
			input: "mac=72:e2:03:9b:d8:0d,addr=172.17.0.2/16",
			want: networks{
				{
					MAC:   net.HardwareAddr{0x72, 0xe2, 0x03, 0x9b, 0xd8, 0x0d},
					Addr4: netip.MustParsePrefix("172.17.0.2/16"),
				},
			},
		},
		{
			name:  "ipv6 only",
			input: "mac=72:e2:03:9b:d8:0d,addr=fd06:322:d419::2/64",
			want: networks{
				{
					MAC:   net.HardwareAddr{0x72, 0xe2, 0x03, 0x9b, 0xd8, 0x0d},
					Addr6: netip.MustParsePrefix("fd06:322:d419::2/64"),
				},
			},
		},
		{
			name:  "dual stack",
			input: "mac=72:e2:03:9b:d8:0d,addr=172.17.0.2/16,addr=fd06:322:d419::2/64",
			want: networks{
				{
					MAC:   net.HardwareAddr{0x72, 0xe2, 0x03, 0x9b, 0xd8, 0x0d},
					Addr4: netip.MustParsePrefix("172.17.0.2/16"),
					Addr6: netip.MustParsePrefix("fd06:322:d419::2/64"),
				},
			},
		},
		{
			name:  "with dhcp",
			input: "mac=72:e2:03:9b:d8:0d,dhcp=true",
			want: networks{
				{
					MAC:  net.HardwareAddr{0x72, 0xe2, 0x03, 0x9b, 0xd8, 0x0d},
					DHCP: true,
				},
			},
		},
	}

	for _, tc := range testscases {
		t.Run(tc.name, func(t *testing.T) {
			var n networks
			err := n.Set(tc.input)
			assert.NoError(t, err)
			assert.Equal(t, tc.want, n)
			// Try to convert back the parsed struct into a string to check if it matches the input.
			assert.Equal(t, tc.input, n.String())
		})
	}
}
