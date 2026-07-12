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

package task

import (
	"context"
	"fmt"
	"sync"

	"github.com/containerd/ttrpc"

	podnsAPI "github.com/containerd/nerdbox/api/services/podns/v1"
)

// sharedNamespaces lazily calls the guest's PodNamespaces.EnsureNamespaces
// TTRPC method the first time it's needed, and memoizes the result. A
// value is created fresh per Task.Create call (see createSandboxedContainer)
// so that a container whose spec never asks for PID/IPC sharing never
// triggers the guest RPC (and, transitively, never causes the guest to
// spawn the PID namespace's anchor process — see internal/vminit/podns
// and internal/vminit/podpause) at all.
type sharedNamespaces struct {
	client *ttrpc.Client // vminitd's TTRPC connection

	once             sync.Once
	ipcPath, pidPath string
	err              error
}

// get implements sharedNamespacesFunc (see podnetns.go).
func (n *sharedNamespaces) get(ctx context.Context) (ipcPath, pidPath string, err error) {
	n.once.Do(func() {
		c := podnsAPI.NewTTRPCPodNamespacesClient(n.client)
		resp, e := c.EnsureNamespaces(ctx, &podnsAPI.EnsureNamespacesRequest{})
		if e != nil {
			n.err = fmt.Errorf("guest EnsureNamespaces: %w", e)
			return
		}
		n.ipcPath = resp.GetIpcNamespacePath()
		n.pidPath = resp.GetPidNamespacePath()
	})
	return n.ipcPath, n.pidPath, n.err
}
