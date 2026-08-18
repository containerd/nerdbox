//go:build !linux

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

package shimtest

import "testing"

// The fixed slirp4netns addresses referenced by network_suite.go; see
// helpers_netns_linux.go. Declared here too so non-Linux builds compile;
// attachContainerNetwork always skips before any of them are used.
const (
	slirpHostAddr = "10.0.2.2"
	slirpDNSAddr  = "10.0.2.3"
)

type containerNetwork struct{}

func attachContainerNetwork(tb testing.TB, _ uint32) *containerNetwork {
	tb.Helper()
	tb.Skip("net tests requiring in-test container networking are Linux-only")
	return nil
}

func (n *containerNetwork) AddInboundForward(tb testing.TB, _, _ int) {
	tb.Helper()
}
