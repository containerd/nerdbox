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

// NetworkSandbox represents the host-side network isolation resource
// associated with a sandbox.  The concept is intentionally abstract so that
// it can be represented differently on each platform:
//
//   - Linux: a bind-mounted network namespace file path.  The caller (CRI)
//     creates and owns the netns; the sandbox holds it open for the lifetime
//     of the sandbox so that CNI and other host-side tools can inspect or
//     manipulate it after the sandbox process has started.
//   - Other platforms: the concept does not exist; the zero-value (NoNetworkSandbox)
//     represents the absence of a network sandbox, which is also the host-network
//     (no isolation) case on Linux.
//
// NetworkSandbox is used as the cross-platform public interface for the
// network sandbox lifecycle.  Platform-specific implementations satisfy it.
type NetworkSandbox interface {
	// Path returns the platform-specific path that identifies the network
	// sandbox.  On Linux this is the network namespace file path.
	// Returns an empty string when there is no network sandbox (host network).
	Path() string

	// Close releases any host-side resources held by the NetworkSandbox.
	// Calling Close on a NoNetworkSandbox is a no-op.
	Close() error
}

// NoNetworkSandbox is a NetworkSandbox that represents the absence of any
// host-side network isolation — used for host-network pods or on platforms
// that do not support network namespaces.
type NoNetworkSandbox struct{}

// Path returns an empty string (no network sandbox).
func (NoNetworkSandbox) Path() string { return "" }

// Close is a no-op.
func (NoNetworkSandbox) Close() error { return nil }

// openNetworkSandbox is the platform-specific factory.  It is defined
// in networksandbox_linux.go (real netns FD) and
// networksandbox_other.go (no-op NoNetworkSandbox).
var openNetworkSandbox func(path string) (NetworkSandbox, error)
