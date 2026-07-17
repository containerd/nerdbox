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

// Package shim_test runs the shimtest conformance suites against the
// nerdbox shim (containerd-shim-nerdbox-v1).
//
// Individual suites or subtests can be selected with -run, e.g.:
//
//	go test -v ./test/shim/... -run TestShim/Exec
//	task test:shim -- -run TestShim/Lifecycle -v
//
// The shim binary is resolved in this order:
//  1. SHIM_BINARY env var (absolute path or name resolved via PATH)
//  2. <module-root>/_output/containerd-shim-nerdbox-v1 — built by "task build:shim"
package shim_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/shimtest"
)

const shimBinaryName = "containerd-shim-nerdbox-v1"

// shimBinaryPath returns the absolute path to the nerdbox shim binary.
// It checks SHIM_BINARY first, then falls back to _output/ in the module root.
func shimBinaryPath() string {
	if v := os.Getenv("SHIM_BINARY"); v != "" {
		return v
	}
	return filepath.Join(moduleRoot(), "_output", shimBinaryName)
}

// shimConfig returns the shimtest Config for nerdbox. The PATH passed to
// the shim process is extended with candidate _output directories so that
// co-located binaries (nerdbox-kernel-*, nerdbox-rootfs.erofs, libkrun.so) are
// found without callers needing to configure anything.
func shimConfig() shimtest.Config {
	return shimtest.Config{
		ShimBinary:   shimBinaryPath(),
		FormatMounts: true,
		Env:          map[string]string{"PATH": shimPath()},
	}
}

func TestMain(m *testing.M) {
	// Register OCI spec types with typeurl (normally done by containerd
	// client init). Required before any shimtest suite runs.
	const prefix = "types.containerd.io"
	major := strconv.Itoa(specs.VersionMajor)
	typeurl.Register(&specs.Process{}, prefix, "opencontainers/runtime-spec", major, "Process")

	os.Exit(m.Run())
}

// TestShim runs the shimtest conformance suites against the nerdbox shim.
// Individual suites and subtests can be targeted with -run, e.g.:
//
//	-run TestShim/Exec
//	-run TestShim/Lifecycle
//
// LayersSuite (HundredLayers) packs 101 erofs layers into a single
// GPT-partitioned VMDK, consuming only one virtio-blk device regardless
// of layer count.
//
// NetworkSuite verifies a container's default outbound network
// connectivity (TCP, UDP, DNS), independent of the mechanism a shim uses
// to provide it. This is the regression guard for TSI (Transparent Socket
// Impersonation), the default connectivity path for containers started
// without any network configuration.
func TestShim(t *testing.T) {
	cfg := shimConfig()
	shimtest.NewRunSuite(cfg).Run(t)
	shimtest.NewExecSuite(cfg).Run(t)
	shimtest.NewTransferSuite(cfg).Run(t)
	shimtest.NewOOMSuite(cfg).Run(t)
	shimtest.NewUDSSuite(cfg).Run(t)
	shimtest.NewLayersSuite(cfg).Run(t)
	shimtest.NewNetworkSuite(cfg).Run(t)
}

// FuzzTransferMissing exercises the transfer service with arbitrary paths
// that don't exist in the container. It verifies the shim returns an
// application-level error rather than hanging.
func FuzzTransferMissing(f *testing.F) {
	shimtest.NewTransferSuite(shimConfig()).Fuzz(f)
}

// shimPath returns a PATH value that prepends candidate _output directories
// to the current PATH. The local module _output/ is highest priority, followed
// by sibling worktree _output/ directories (to find kernel/initrd/libkrun built
// in another branch worktree).
func shimPath() string {
	root := moduleRoot()
	current := os.Getenv("PATH")

	// Build the final PATH as an ordered, deduplicated list:
	//   1. local _output (always first, re-anchored even if already present)
	//   2. sibling worktree _output dirs (fallback for kernel/initrd/libkrun)
	//   3. everything already in PATH, minus any entries already added above
	//
	// The local _output must be unconditionally first: shimtest helpers call
	// os.Setenv to inject it into the test-process PATH between tests, so by
	// the time shimPath is called again it may already be present — but
	// sibling dirs may also have been added and could sort ahead of it.
	localOutput := filepath.Join(root, "_output")

	parent := filepath.Dir(root)
	var siblingOutputs []string
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			if !e.IsDir() || e.Name() == filepath.Base(root) {
				continue
			}
			siblingOutputs = append(siblingOutputs, filepath.Join(parent, e.Name(), "_output"))
		}
	}

	seen := make(map[string]bool)
	var result []string

	add := func(dir string) {
		if !seen[dir] {
			seen[dir] = true
			result = append(result, dir)
		}
	}

	// 1. Local _output first (exists check; silently skip if missing).
	if _, err := os.Stat(localOutput); err == nil {
		add(localOutput)
	}
	// 2. Sibling _output dirs that exist and haven't been added yet.
	for _, dir := range siblingOutputs {
		if _, err := os.Stat(dir); err == nil {
			add(dir)
		}
	}
	// 3. Retain existing PATH entries not already included above.
	for _, dir := range filepath.SplitList(current) {
		add(dir)
	}

	return strings.Join(result, string(os.PathListSeparator))
}

// moduleRoot returns the absolute path to the module root directory.
// This source file is at <module-root>/test/shim/shim_test.go,
// so we walk up two directories.
func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
