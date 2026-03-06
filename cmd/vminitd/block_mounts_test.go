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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseBlockMounts(t *testing.T) {
	testcases := []struct {
		name    string
		inputs  []string
		want    blockMounts
		wantStr string
	}{
		{
			name:   "single block mount",
			inputs: []string{"/dev/vdc:/mnt/sdc"},
			want: blockMounts{
				{source: "/dev/vdc", target: "/mnt/sdc", readonly: false},
			},
			wantStr: "/dev/vdc:/mnt/sdc",
		},
		{
			name:   "single block mount read-only",
			inputs: []string{"/dev/vdc:/mnt/sdc:ro"},
			want: blockMounts{
				{source: "/dev/vdc", target: "/mnt/sdc", readonly: true},
			},
			wantStr: "/dev/vdc:/mnt/sdc:ro",
		},
		{
			name:   "multiple block mounts",
			inputs: []string{"/dev/vdc:/mnt/sdc", "/dev/vdd:/mnt/sdd"},
			want: blockMounts{
				{source: "/dev/vdc", target: "/mnt/sdc", readonly: false},
				{source: "/dev/vdd", target: "/mnt/sdd", readonly: false},
			},
			wantStr: "/dev/vdc:/mnt/sdc,/dev/vdd:/mnt/sdd",
		},
		{
			name:   "multiple block mounts mixed read-write and read-only",
			inputs: []string{"/dev/vdc:/mnt/sdc", "/dev/vdd:/mnt/sdd:ro"},
			want: blockMounts{
				{source: "/dev/vdc", target: "/mnt/sdc", readonly: false},
				{source: "/dev/vdd", target: "/mnt/sdd", readonly: true},
			},
			wantStr: "/dev/vdc:/mnt/sdc,/dev/vdd:/mnt/sdd:ro",
		},
		{
			name:   "block mount with nested target path",
			inputs: []string{"/dev/vdc:/mnt/sdc/data"},
			want: blockMounts{
				{source: "/dev/vdc", target: "/mnt/sdc/data", readonly: false},
			},
			wantStr: "/dev/vdc:/mnt/sdc/data",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var b blockMounts
			for _, input := range tc.inputs {
				err := b.Set(input)
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.want, b)
			assert.Equal(t, tc.wantStr, b.String())
		})
	}
}

func TestParseBlockMountsError(t *testing.T) {
	testcases := []struct {
		name  string
		input string
	}{
		{
			name:  "missing colon separator",
			input: "/dev/vdc",
		},
		{
			name:  "empty source",
			input: ":/mnt/sdc",
		},
		{
			name:  "empty target",
			input: "/dev/vdc:",
		},
		{
			name:  "empty string",
			input: "",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var b blockMounts
			err := b.Set(tc.input)
			assert.Error(t, err)
		})
	}
}
