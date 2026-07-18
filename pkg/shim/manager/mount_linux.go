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

package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// cloneMntNs configures the child command to start in a new mount
// namespace, and — only when the calling process is not already real
// root — a new user namespace as well. The user namespace provides mount
// isolation and grants the child capabilities within it, without
// requiring or granting real host capabilities. User namespaces are
// available unprivileged on many distros (since Linux 3.8), but some may
// gate them via sysctl (e.g. kernel.apparmor_restrict_unprivileged_userns
// on Ubuntu).
//
// For a VM-based runtime like nerdbox, the shim does not need real host
// root — it needs /dev/kvm access (checked against mapped host UID) and
// file access (same user). The user namespace is defense-in-depth: it
// limits the shim's host-level capabilities even when the daemon runs as
// root.
//
// We use clone flags instead of unshare(2) because unshare(CLONE_NEWUSER)
// requires the calling process to be single-threaded, which is not
// possible in a Go program (the runtime uses multiple OS threads).
//
// The new mount namespace inherits copies of the parent's mounts with
// the same propagation flags. The shim performs rootfs mounts (overlay /
// bind) inside this namespace. On hosts where / is shared, those mounts
// could in theory propagate back. Because the child also runs in a user
// namespace, it cannot remount / as MS_SLAVE. In practice this is safe:
// the mounts are into bundle-specific paths that are cleaned up on
// container delete, and the VM itself performs all container-visible
// filesystem setup.
//
// The UID/GID mapping maps container-side 0 to the real host UID/GID,
// so the child appears as root *only inside its own, brand-new user
// namespace*. This grants no additional real host privilege: every
// interaction with a resource outside the namespace (files, sockets
// inherited across the namespace boundary, etc.) is still translated
// back through the mapping to the real, unprivileged host UID for
// permission checks.
//
// Mapping to UID 0 (rather than mapping the host UID to itself, which
// would leave euid non-zero inside the new namespace) matters because of
// how Linux computes capabilities across exec: a process whose effective
// UID is non-zero *within its own current user namespace* has its
// capability sets cleared to empty when it execs, even though the
// namespace's creator normally holds a full capability set in it. Since
// this child is always exec'd into the new namespace (see above), a
// non-zero-inside-its-own-namespace mapping would leave it with no
// capabilities at all afterward — unable to perform the bind mounts
// SharedFS needs, or even call getsockopt(2) on a listening-socket fd
// inherited across the namespace boundary (reproduced standalone by
// script/userns-check). Mapping to UID 0 keeps the child's effective UID
// zero *inside its own namespace* across exec, so the capability set is
// preserved and mount(2)/getsockopt(2) work as expected — with no change
// to what the process can do to real host resources, which remain gated
// by the real, unprivileged host UID/GID the mapping points at.
//
// When the calling process already has real root (euid 0), we deliberately
// skip CLONE_NEWUSER: entering a *new* user namespace — even one that maps
// UID 0 to the real root UID — demotes the process to a non-initial user
// namespace, and the kernel restricts mounting real block-device-backed
// filesystems (e.g. ext4) to the initial user namespace regardless of the
// effective capabilities held within a descendant namespace. Real root
// gets CLONE_NEWNS alone, which still provides the mount-namespace
// isolation/cleanup-on-exit benefit without losing the ability to mount
// real filesystems.
//
// If namespace creation is not possible (e.g. AppArmor restricts
// unprivileged user namespaces), the shim runs without mount isolation
// and this function returns false.
// cloneMntNs returns true if user namespace clone flags were set.
func cloneMntNs(_ context.Context, cmd *exec.Cmd) bool {
	if os.Geteuid() == 0 {
		// Already real root: a plain mount namespace is enough, and
		// avoids demoting into a non-initial user namespace (which would
		// break mounts of real block-device filesystems).
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWNS
		return false
	}

	if restricted, err := apparmorRestrictsUserns(); err != nil {
		// Failed to check apparmor userns restriction, skipping mount namespace isolation")
		// We can't log anything here as it will break the TTRPC protocol!
		return false
	} else if restricted {
		// apparmor_restrict_unprivileged_userns=1 prevents user namespace creation; shim will run without mount namespace isolation
		// We can't log anything here as it will break the TTRPC protocol!
		return false
	}

	uid := os.Getuid()
	gid := os.Getgid()
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: uid, Size: 1},
	}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: gid, Size: 1},
	}
	return true
}

// apparmorRestrictsUserns checks if the kernel sysctl
// kernel.apparmor_restrict_unprivileged_userns is set to 1.
// Returns (false, nil) when the sysctl does not exist (older kernels or
// AppArmor not enabled).
func apparmorRestrictsUserns() (bool, error) {
	data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(data)) == "1" {
		if runningUnderCustomProfile() {
			return false, nil
		}
		return true, nil
	}

	return false, nil
}

// runningUnderCustomProfile checks /proc/self/attr/current for an AppArmor
// profile name that contains the current binary name. If present, we are
// likely running under a custom profile that relaxes the userns restriction
// for this binary, so we should proceed with namespace creation.
func runningUnderCustomProfile() bool {
	data, err := os.ReadFile("/proc/self/attr/current")
	if err != nil {
		return false
	}
	profile := strings.TrimSpace(string(data))
	return strings.Contains(profile, filepath.Base(os.Args[0]))
}
