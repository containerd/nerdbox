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

func TestParseBindMounts(t *testing.T) {
	testcases := []struct {
		name    string
		inputs  []string
		want    bindMounts
		wantStr string
	}{
		{
			name:   "single bind mount",
			inputs: []string{"foo:/mnt/foo"},
			want: bindMounts{
				{tag: "foo", target: "/mnt/foo"},
			},
			wantStr: "foo:/mnt/foo",
		},
		{
			name:   "multiple bind mounts",
			inputs: []string{"foo:/mnt/foo", "bar:/mnt/bar"},
			want: bindMounts{
				{tag: "foo", target: "/mnt/foo"},
				{tag: "bar", target: "/mnt/bar"},
			},
			wantStr: "foo:/mnt/foo,bar:/mnt/bar",
		},
		{
			name:   "bind mount with nested path",
			inputs: []string{"config:/mnt/etc/config"},
			want: bindMounts{
				{tag: "config", target: "/mnt/etc/config"},
			},
			wantStr: "config:/mnt/etc/config",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var b bindMounts
			for _, input := range tc.inputs {
				err := b.Set(input)
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.want, b)
			// Try to convert back the parsed struct into a string to check if it matches the expected output.
			assert.Equal(t, tc.wantStr, b.String())
		})
	}
}

func TestParseBindMountsError(t *testing.T) {
	testcases := []struct {
		name  string
		input string
	}{
		{
			name:  "missing target",
			input: "foo",
		},
		{
			name:  "empty tag",
			input: ":foo",
		},
		{
			name:  "empty target",
			input: "foo:",
		},
		{
			name:  "empty string",
			input: "",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var b bindMounts
			err := b.Set(tc.input)
			assert.Error(t, err)
		})
	}
}
