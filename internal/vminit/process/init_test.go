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

package process

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	runc "github.com/containerd/go-runc"
	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"

	"github.com/containerd/nerdbox/internal/mountutil"
)

// Exercise the real guest mount setup and Init.delete, in a private mount
// namespace. Only the OCI runtime command is replaced: there is no process to
// delete, but Nerdbox must still remove every rootfs component it mounted.
func TestInitDeleteUnmountsRootfsComponents(t *testing.T) {
	const child = "NERDBOX_TEST_ROOTFS_UNMOUNT_CHILD"
	if os.Getenv(child) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestInitDeleteUnmountsRootfsComponents$", "-test.v")
		cmd.Env = append(os.Environ(), child+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
		output, err := cmd.CombinedOutput()
		if errors.Is(err, unix.EPERM) && os.Getenv("NERDBOX_REQUIRE_MOUNT_TEST") != "1" {
			t.Skip("requires CAP_SYS_ADMIN for an isolated mount namespace")
		}
		if err != nil {
			t.Fatalf("guest cleanup regression: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}

	bundle := t.TempDir()
	rootfs := filepath.Join(bundle, "rootfs")
	components := filepath.Join(bundle, "mounts")
	if err := os.Mkdir(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	// Cleanup is deliberately broader than production's current delete: leave
	// the test namespace clean even when the regression assertion fails.
	t.Cleanup(func() {
		if err := mount.UnmountRecursive(bundle, 0); err != nil {
			t.Errorf("test mount cleanup: %v", err)
		}
	})
	mounts := []*types.Mount{
		{Type: "tmpfs", Source: "tmpfs", Options: []string{"size=4m"}},
		{Type: "tmpfs", Source: "tmpfs", Options: []string{"size=4m"}},
		{
			Type:   "format/mkdir/overlay",
			Source: "overlay",
			Options: []string{
				"X-containerd.mkdir.path={{ mount 0 }}/upper:0755",
				"X-containerd.mkdir.path={{ mount 0 }}/work:0755",
				"upperdir={{ mount 0 }}/upper",
				"workdir={{ mount 0 }}/work",
				"lowerdir={{ mount 1 }}",
			},
		},
	}
	if err := mountutil.All(t.Context(), rootfs, components, mounts); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "marker"), []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	init := &Init{
		id:      "cleanup-test",
		Bundle:  bundle,
		Rootfs:  rootfs,
		runtime: &runc.Runc{Command: executable, ExtraArgs: []string{"-test.run=^TestInitDeleteRuntime$", "--"}},
	}
	if err := init.delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	remaining, err := mountinfo.GetMounts(mountinfo.PrefixFilter(bundle))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range remaining {
		t.Errorf("Init.delete left rootfs component mounted: %s (%s)", entry.Mountpoint, entry.FSType)
	}
}

// The subprocess stands in for a successful OCI runtime delete. Actual
// filesystem setup and teardown remain the production Nerdbox code above.
func TestInitDeleteRuntime(t *testing.T) {
	if !strings.HasSuffix(strings.Join(os.Args, " "), "-- delete cleanup-test") {
		t.Skip("OCI runtime subprocess only")
	}
}
