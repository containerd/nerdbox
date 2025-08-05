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

package integration

import (
	"testing"

	systemapi "github.com/dmcgowan/nerdbox/api/services/system/v1"
)

func TestSystemInfo(t *testing.T) {
	client := startVM(t)

	ss := systemapi.NewTTRPCSystemClient(client)

	resp, err := ss.Info(t.Context(), nil)
	if err != nil {
		t.Fatal("failed to get system info:", err)
	}
	if resp.Version != "dev" {
		t.Fatalf("unexpected version: %s, expected: dev", resp.Version)
	}
	t.Log("Kernel Version:", resp.KernelVersion)
}
