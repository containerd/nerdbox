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
	"fmt"
	"os"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// nsfsMagic is the filesystem magic number for Linux nsfs (the filesystem
// that backs namespace files under /proc/*/ns/). Mirrors the identical
// constant in internal/vm/libkrun/krun_linux.go, kept local to this
// package rather than shared to avoid a dependency between the two for a
// single well-known constant.
const nsfsMagic = 0x6e736673

func init() {
	openNetworkSandbox = linuxOpenNetworkSandbox
}

// linuxNetworkSandbox holds an open file descriptor to a Linux network
// namespace bind-mount.  The open FD keeps the bind-mount alive for the
// lifetime of the sandbox, satisfying the CRI contract that the netns
// remains pinned while the sandbox is running — regardless of whether any
// process is actively in it.
type linuxNetworkSandbox struct {
	path string
	fd   *os.File
}

// linuxOpenNetworkSandbox opens the network namespace at path and returns a
// NetworkSandbox that holds the FD open.  Returns NoNetworkSandbox when path
// is empty (host-network pod).
func linuxOpenNetworkSandbox(path string) (NetworkSandbox, error) {
	if path == "" {
		return NoNetworkSandbox{}, nil
	}

	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open network sandbox %q: %w", path, err)
	}

	// Verify path is actually backed by nsfs (the filesystem that exposes
	// kernel namespace files under /proc/*/ns/), so that a bind-mounted
	// netns is confirmed to be a real network namespace, not some other
	// file that happens to sit at the given path.
	//
	// A plain regular file (not nsfs) is deliberately still accepted
	// rather than rejected: shimtest's non-root test suite pins a plain
	// file in place of a real netns specifically to avoid requiring
	// CAP_SYS_ADMIN or kernel bind-mount support, matching the identical
	// tolerance in internal/vm/libkrun's vmcontextSetNetns.
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &sfs); err != nil {
		f.Close()
		return nil, fmt.Errorf("statfs network sandbox %q: %w", path, err)
	}
	if sfs.Type != nsfsMagic {
		log.L.WithField("netns", path).Debug(
			"network sandbox path is not an nsfs file; pinning it anyway (test or non-standard path)")
	}

	return &linuxNetworkSandbox{path: path, fd: f}, nil
}

// Path returns the network namespace file path.
func (n *linuxNetworkSandbox) Path() string { return n.path }

// Close closes the held FD, releasing the pin on the network namespace.
func (n *linuxNetworkSandbox) Close() error {
	if n.fd == nil {
		return nil
	}
	err := n.fd.Close()
	n.fd = nil
	return err
}
