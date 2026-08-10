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

	srAPI "github.com/containerd/nerdbox/api/services/sharedresources/v1"
)

// sharedResourceFunc returns the guest paths of the sandbox's shared
// resources of the requested types, creating them on first use. It is
// called by sanitizeNamespaces at most once per container, and only if
// that container's spec actually asks to share something.
type sharedResourceFunc func(ctx context.Context, types []srAPI.Type) (map[srAPI.Type]string, error)

// sharedResources calls the guest's SharedResources.Create the first time a
// given type is needed and memoizes the result per type. A value is
// created fresh per Task.Create call (see createSandboxedContainer), so a
// container that shares nothing never triggers the guest RPC at all — and
// therefore never causes the guest to create a namespace, or to spawn the
// PID namespace's anchor process, on its behalf.
type sharedResources struct {
	client    *ttrpc.Client // vminitd's TTRPC connection
	sandboxID string        // resource group id

	mu    sync.Mutex
	paths map[srAPI.Type]string
}

// get implements sharedResourceFunc. Types already fetched are served from
// the memo; only the remainder is requested from the guest.
func (n *sharedResources) get(ctx context.Context, types []srAPI.Type) (map[srAPI.Type]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	var missing []srAPI.Type
	for _, t := range types {
		if _, ok := n.paths[t]; !ok {
			missing = append(missing, t)
		}
	}

	if len(missing) > 0 {
		c := srAPI.NewTTRPCSharedResourcesClient(n.client)
		resp, err := c.Create(ctx, &srAPI.CreateRequest{
			ID:    n.sandboxID,
			Types: missing,
		})
		if err != nil {
			return nil, fmt.Errorf("guest resource create: %w", err)
		}
		if n.paths == nil {
			n.paths = make(map[srAPI.Type]string, len(missing))
		}
		for _, r := range resp.GetResources() {
			n.paths[r.GetType()] = r.GetPath()
		}
	}

	out := make(map[srAPI.Type]string, len(types))
	for _, t := range types {
		path, ok := n.paths[t]
		if !ok {
			return nil, fmt.Errorf("guest did not return a path for resource type %q", t)
		}
		out[t] = path
	}
	return out, nil
}

// getDevShm returns the guest path of the sandbox's shared /dev/shm tmpfs,
// creating it (sized to sizeBytes) on first use. It shares this
// sharedResources instance's memo and guest connection with get, but is
// called independently by shareDevShmMounter rather than as part of
// sanitizeNamespaces's batched resource request, since a container may
// need one without the other.
//
// sizeBytes is only used the first time this sandbox creates its shared
// /dev/shm; see SharedResources's CreateRequest.dev_shm_size_bytes doc
// comment for why a later, different value is silently ignored.
func (n *sharedResources) getDevShm(ctx context.Context, sizeBytes int64) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	const typ = srAPI.Type_TYPE_DEVSHM
	if path, ok := n.paths[typ]; ok {
		return path, nil
	}

	c := srAPI.NewTTRPCSharedResourcesClient(n.client)
	resp, err := c.Create(ctx, &srAPI.CreateRequest{
		ID:              n.sandboxID,
		Types:           []srAPI.Type{typ},
		DevShmSizeBytes: sizeBytes,
	})
	if err != nil {
		return "", fmt.Errorf("guest devshm create: %w", err)
	}
	for _, r := range resp.GetResources() {
		if r.GetType() != typ {
			continue
		}
		if n.paths == nil {
			n.paths = make(map[srAPI.Type]string, 1)
		}
		n.paths[typ] = r.GetPath()
		return r.GetPath(), nil
	}
	return "", fmt.Errorf("guest did not return a path for devshm resource")
}
