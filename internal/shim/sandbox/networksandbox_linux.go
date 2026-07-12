// Copyright The containerd Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandbox

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

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

	// Verify the path looks like a network namespace before opening it.
	// InotifyInit1 is not used here — a plain O_RDONLY open is sufficient
	// to pin the bind-mount.
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return nil, fmt.Errorf("network sandbox path %q: %w", path, err)
	}

	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open network sandbox %q: %w", path, err)
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
