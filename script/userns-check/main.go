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

// Detects whether user namespaces are restricted by checking if
// getsockopt(SO_TYPE) returns EACCES when a unix socket fd is inherited
// by a child spawned with CLONE_NEWUSER + a UID mapping + exec.
//
// This reproduces the exact failure path in the nerdbox shim where
// net.FileListener calls getsockopt(fd, SOL_SOCKET, SO_TYPE) and gets EACCES.
//
// The exec is critical: it triggers capability recomputation. With euid != 0
// in the new userns, caps drop to zero, and cross-userns socket access fails.
//
// Exit codes:
//
//	0  — userns NOT restricted (getsockopt succeeded)
//	1  — userns RESTRICTED (getsockopt got EACCES/EPERM)
//	2  — unexpected error
//	77 — skipped (running as root)
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--child" {
		os.Exit(childMain())
	}
	os.Exit(parentMain())
}

func childMain() int {
	fd := 3
	fmt.Printf("child: pid=%d uid=%d, calling getsockopt(fd=%d, SO_TYPE)\n",
		os.Getpid(), os.Getuid(), fd)

	_, err := unix.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		fmt.Printf("child: getsockopt FAILED: %v\n", err)
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			return 1
		}
		return 2
	}

	fmt.Printf("child: getsockopt OK\n")
	return 0
}

func parentMain() int {
	uid := os.Getuid()
	gid := os.Getgid()

	if uid == 0 {
		fmt.Fprintf(os.Stderr, "run as non-root to reproduce (root gets caps in new userns)\n")
		return 77
	}

	// Create a Unix socket in a temp directory — same idea as the shim's
	// listener socket.
	tmpDir, err := os.MkdirTemp("", "userns-check-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdirtemp: %v\n", err)
		return 2
	}
	defer os.RemoveAll(tmpDir)

	sockPath := filepath.Join(tmpDir, "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		return 2
	}
	defer ln.Close()

	f, err := ln.(*net.UnixListener).File()
	if err != nil {
		fmt.Fprintf(os.Stderr, "file: %v\n", err)
		return 2
	}
	defer f.Close()

	fmt.Printf("parent: created socket fd=%d (uid=%d)\n", f.Fd(), uid)

	// Re-exec ourselves as "--child" with CLONE_NEWUSER|CLONE_NEWNS.
	// This is the same clone+exec pattern Go's ForkExec uses when
	// SysProcAttr.Cloneflags is set — which triggers cap recomputation.
	// The UID/GID mappings mirror the shim's cloneMntNs implementation.
	cmd := exec.Command("/proc/self/exe", "--child")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{f} // fd 3 in child
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: uid, HostID: uid, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: gid, HostID: gid, Size: 1},
		},
	}

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			rc := exitErr.ExitCode()
			if rc == 1 {
				fmt.Printf("USERNS RESTRICTED — child got EACCES/EPERM (CLONE_NEWUSER + exec)\n")
				return 1
			}
			fmt.Printf("unexpected child failure (exit %d)\n", rc)
			return 2
		}
		fmt.Fprintf(os.Stderr, "exec: %v\n", err)
		return 2
	}

	fmt.Printf("userns NOT restricted — getsockopt succeeded\n")
	return 0
}
