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

// Package podnetns creates the persistent, guest-side network namespace
// that all member containers of a sandbox join by default (see
// internal/podnetns for the shared path/name constants and the rationale).
package podnetns

import (
	"context"
	"fmt"
	"runtime"

	"github.com/containerd/log"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/containerd/nerdbox/internal/podnetns"
)

// Create creates the persistent, named guest network namespace at
// podnetns.Path and brings up its loopback interface. It must be called
// once at vminitd startup, before any container is created.
//
// This uses the same "persistent netns" technique containerd's CRI plugin
// uses on the host (see docs/sandbox-architecture.md, Layer 1): a
// dedicated goroutine locks itself to an OS thread, unshares a new network
// namespace on that thread, and bind-mounts it to a well-known path. The
// bind-mount is what keeps the namespace alive; the creating goroutine does
// not need to stay alive afterward, and Go retires the underlying OS thread
// when it exits (Go 1.10+), so there is no thread-pool "poisoning" concern.
//
// No explicit teardown is provided or needed: the namespace and its
// bind-mount are guest kernel state, which disappears entirely when the VM
// shuts down.
func Create(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Intentionally no UnlockOSThread: seeing this comment's sibling in
		// internal/vminit/ctrnetworking and shimtest's realnetns helpers for
		// the same pattern.

		if _, err := netns.NewNamed(podnetns.Name); err != nil {
			errCh <- fmt.Errorf("create pod netns %q: %w", podnetns.Path, err)
			return
		}

		// NewNamed leaves this (locked) thread's current namespace set to
		// the newly created one, so a plain netlink.LinkByName operates
		// inside it without needing a NewHandleAt.
		link, err := netlink.LinkByName("lo")
		if err != nil {
			errCh <- fmt.Errorf("lookup lo in pod netns: %w", err)
			return
		}
		if err := netlink.LinkSetUp(link); err != nil {
			errCh <- fmt.Errorf("bring up lo in pod netns: %w", err)
			return
		}
		log.G(ctx).WithField("path", podnetns.Path).Debug("created pod network namespace")
		errCh <- nil
	}()
	return <-errCh
}
