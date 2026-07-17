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

package libkrun

import (
	"errors"
	"fmt"
	"os"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// nsfsMagic is the filesystem magic number for Linux nsfs (the filesystem that
// backs namespace files under /proc/*/ns/).
const nsfsMagic = 0x6e736673

// vmcontextSetNetns enters the network namespace at path on the calling OS
// thread using setns(2).  It must be called from the locked OS thread that
// is about to call krun_start_enter (see vmInstance.Start in instance.go)
// so that all worker threads libkrun spawns from that thread (vCPU, virtio
// backends, vsock/TSI workers) inherit the namespace.
//
// The file descriptor is opened O_RDONLY|O_CLOEXEC, used for setns, and then
// closed — the netns is pinned by the bind-mount at path (managed by the CRI
// layer), not by this FD.
//
// The function checks that path refers to a real network namespace file (nsfs
// magic).  If not (e.g. a plain file used in tests), it returns nil without
// attempting setns.
//
// If setns fails with EPERM (the shim lacks CAP_SYS_ADMIN in the initial user
// namespace), the error is logged at warning level and the function returns
// nil.  VM traffic will then originate from the shim's own network namespace
// rather than the pod netns, matching the previous behaviour.  In production,
// containerd runs the shim as root, so setns succeeds.
func vmcontextSetNetns(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open netns %q: %w", path, err)
	}
	defer f.Close()

	// Check whether path is a real nsfs file.  A plain file (e.g. one
	// created for testing) has a different filesystem magic and cannot be
	// used with setns; skip silently in that case.
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &sfs); err != nil {
		return fmt.Errorf("statfs netns %q: %w", path, err)
	}
	if sfs.Type != nsfsMagic {
		log.L.WithField("netns", path).Debug(
			"netns path is not an nsfs file; skipping setns (test or non-standard path)")
		return nil
	}

	if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
		if errors.Is(err, unix.EPERM) {
			// Log and continue: shim lacks CAP_SYS_ADMIN; VM traffic will
			// use the shim's own netns instead of the pod netns.
			log.L.WithField("netns", path).Warn(
				"setns into pod netns not permitted (shim not running as root); " +
					"VM traffic will use shim netns")
			return nil
		}
		return fmt.Errorf("setns %q: %w", path, err)
	}
	return nil
}
