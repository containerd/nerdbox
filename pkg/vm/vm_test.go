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

package vm

import "testing"

// TestWithNetNS verifies that WithNetNS sets StartOpts.NetNS.
func TestWithNetNS(t *testing.T) {
	var o StartOpts
	WithNetNS("/run/netns/foo")(&o)
	if o.NetNS != "/run/netns/foo" {
		t.Fatalf("NetNS = %q, want /run/netns/foo", o.NetNS)
	}
}

// TestWithNetNS_Empty verifies that WithNetNS accepts an empty path
// (host-network).
func TestWithNetNS_Empty(t *testing.T) {
	o := StartOpts{NetNS: "should be overwritten"}
	WithNetNS("")(&o)
	if o.NetNS != "" {
		t.Fatalf("NetNS = %q, want empty", o.NetNS)
	}
}
