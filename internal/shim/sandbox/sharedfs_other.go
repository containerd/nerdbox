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

package sandbox

import (
	"context"
	"fmt"
	"runtime"

	"github.com/containerd/containerd/api/types"
)

// errSharedFSUnsupported is returned by every SharedFS operation that
// requires assembling real host-side mounts (bind/overlay/etc.), which is
// only implemented on Linux today (see sharedfs_linux.go). The sandbox
// (multi-container-per-VM) shim API is Linux-only for now; see
// docs/sandbox-architecture.md.
var errSharedFSUnsupported = fmt.Errorf("sandbox shared filesystem not supported on %s", runtime.GOOS)

// Every stub below takes the same lock the Linux implementation does
// (sharedfs_linux.go) even though there is nothing to mutate, purely so
// that SharedFS.mu has a use on every platform; leaving it genuinely
// unused here would fail the "unused" lint check on non-Linux builds.

func (s *SharedFS) shareRootfs(context.Context, string, []*types.Mount) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return "", errSharedFSUnsupported
}

func (s *SharedFS) shareVolume(context.Context, string, int, string, bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return "", errSharedFSUnsupported
}

func (s *SharedFS) unshare(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errSharedFSUnsupported
}

func (s *SharedFS) unshareAll(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errSharedFSUnsupported
}
