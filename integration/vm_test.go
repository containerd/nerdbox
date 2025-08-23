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
	"errors"
	"testing"

	"github.com/containerd/errdefs"

	systemapi "github.com/dmcgowan/nerdbox/api/services/system/v1"
	"github.com/dmcgowan/nerdbox/internal/vm"
)

func TestSystemInfo(t *testing.T) {
	runWithVM(t, func(t *testing.T, i vm.Instance) {
		client := i.Client()

		ss := systemapi.NewTTRPCSystemClient(client)

		resp, err := ss.Info(t.Context(), nil)
		if err != nil {
			t.Fatal("failed to get system info:", err)
		}
		if resp.Version != "dev" {
			t.Fatalf("unexpected version: %s, expected: dev", resp.Version)
		}
		t.Log("Kernel Version:", resp.KernelVersion)
	})
}

func TestStreamInitialization(t *testing.T) {
	runWithVM(t, func(t *testing.T, i vm.Instance) {
		sid1, conn, err := i.StartStream(t.Context())
		if err != nil {
			if errors.Is(err, errdefs.ErrNotImplemented) {
				t.Skip("streaming not implemented")
			}
			t.Fatal("failed to start stream client:", err)
		}

		if sid1 == 0 {
			t.Fatal("expected non-zero stream id")
		}

		if err := conn.Close(); err != nil {
			t.Fatal("failed to close stream connection:", err)
		}

		sid2, conn, err := i.StartStream(t.Context())
		if err != nil {
			t.Fatal("failed to start stream client:", err)
		}

		if sid2 <= sid1 {
			t.Fatalf("expected stream id %d, previous was %d", sid2, sid1)
		}

		if err := conn.Close(); err != nil {
			t.Fatal("failed to close stream connection:", err)
		}
	})
}
