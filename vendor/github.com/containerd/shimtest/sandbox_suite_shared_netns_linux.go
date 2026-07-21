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

package shimtest

import (
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
)

// testMemberContainersShareNetwork verifies that member containers of the
// same sandbox share a network stack: a listener started by one member
// container is reachable from a second, independently created member
// container via loopback, with no explicit network configuration on either
// container.
//
// The API contract: a shim's sandbox model requires all member containers
// of one sandbox to share a single network identity (e.g. this is what
// backs a Kubernetes pod's shared network namespace). This test only
// observes the externally visible result of that contract —
// cross-container loopback connectivity — and does not assume any
// particular mechanism a shim uses to provide it (a real shared network
// namespace, a shared virtual interface, or any other approach).
func (s *SandboxSuite) testMemberContainersShareNetwork(t *testing.T) {
	sandboxID := containerID(t)
	env := startSandboxShim(t, s.cfg, sandboxID)

	const port = "9192"

	listenerCID := createContainerInSandbox(t, env, []string{"/bin/echosrv", port})

	// Give the listener container a moment to actually bind and start
	// accepting before the client attempts to connect. There is no
	// synchronous "ready" signal available across two containers created
	// independently via the Task API.
	time.Sleep(200 * time.Millisecond)

	const token = "shared-netns-ok"
	clientCID := createContainerInSandbox(t, env, []string{"/bin/nc", "127.0.0.1", port}, withSandboxCtrStdin())
	writeContainerStdin(t, env, clientCID, token)
	readContainerOutput(t, env, clientCID, token, 30*time.Second)

	clientWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: clientCID})
	if err != nil {
		t.Fatalf("Task.Wait client: %v", err)
	}
	if clientWait.GetExitStatus() != 0 {
		t.Fatalf("client container exit status: got %d, want 0", clientWait.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: clientCID}) //nolint:errcheck

	listenerWait, err := env.tc.Wait(env.ctx, &taskAPI.WaitRequest{ID: listenerCID})
	if err != nil {
		t.Fatalf("Task.Wait listener: %v", err)
	}
	if listenerWait.GetExitStatus() != 0 {
		t.Fatalf("listener container exit status: got %d, want 0", listenerWait.GetExitStatus())
	}
	env.tc.Delete(env.ctx, &taskAPI.DeleteRequest{ID: listenerCID}) //nolint:errcheck

	t.Log("member containers share a network namespace: loopback connectivity confirmed")
}
