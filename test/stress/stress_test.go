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

// Package stress_test runs the shimtest stress suites against the
// nerdbox shim (containerd-shim-nerdbox-v1).
//
// Each stress subtest runs until one minute before the -test.timeout
// deadline (default 10m). Run all subtests or select one by name:
//
//	task test:stress                          # all three sequentially, each 10m
//	task test:stress:lifecycle                # lifecycle only
//	task test:stress:exec                     # exec only
//	task test:stress:transfer                 # transfer only
//	task test:stress -- -timeout 20m          # all three with 20m each
//	go test -v -timeout 10m ./test/stress/... -run TestShimStress/Lifecycle
package stress_test

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

// TestShimStress runs the shimtest stress suites against the nerdbox shim.
// Each subtest (Lifecycle, Exec, Transfer, Sandbox) runs until one minute
// before the -test.timeout deadline. Select individual subtests with -run:
//
//	-run TestShimStress/Lifecycle
//	-run TestShimStress/Exec
//	-run TestShimStress/Transfer
//	-run TestShimStress/Sandbox
func TestShimStress(t *testing.T) {
	shimtest.NewStressSuite(shimConfig(), shimtest.StressOptions{
		Transfer: true,
		Sandbox:  true,
		// The sandbox stress test creates thousands of container lifecycles
		// inside a single VM (default 2 GiB guest RAM).  The host shim's RSS
		// grows as the VM progressively faults in guest pages and the Go
		// runtime's heap settles at a high watermark.  This one-time step
		// saturates well below 2 GiB (the full guest RAM) and is not a leak.
		//
		// Observed nerdbox data over a 21-minute / 45K-container run:
		//   RSS before:  ~112 MiB (just sandbox booted)
		//   RSS after:  ~1270 MiB (~20 min)
		// Growth from guest RAM faults saturates; the rate drops after the
		// first few minutes.  3 GiB accommodates this one-time step with
		// headroom; a true per-container leak at the observed ~3 KiB/iter
		// rate would cross 3 GiB only after ~1 million containers.
		SandboxRSSGrowthOverride: 3 * 1024 * 1024 * 1024, // 3 GiB
	}).Run(t)
}

// shimPath returns a PATH value that prepends candidate _output directories
// to the current PATH. The local module _output/ is always first. Sibling
// worktree _output/ directories are included for kernel/rootfs/vminitd
// fallback, but any sibling that contains its own libkrun.so is skipped:
// using a stale libkrun from another worktree can cause symbol-not-found
// crashes (e.g. missing krun_add_virtiofs3).
func shimPath() string {
	root := moduleRoot()
	current := os.Getenv("PATH")
	localOutput := filepath.Join(root, "_output")

	parent := filepath.Dir(root)
	var siblingOutputs []string
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			if !e.IsDir() || e.Name() == filepath.Base(root) {
				continue
			}
			dir := filepath.Join(parent, e.Name(), "_output")
			// Skip sibling _output dirs that carry their own libkrun.so.
			if _, err := os.Stat(filepath.Join(dir, "libkrun.so")); err == nil {
				continue
			}
			siblingOutputs = append(siblingOutputs, dir)
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

	// 1. Local _output first.
	if _, err := os.Stat(localOutput); err == nil {
		add(localOutput)
	}
	// 2. Sibling _output dirs without libkrun.so (kernel/rootfs fallback).
	for _, dir := range siblingOutputs {
		if _, err := os.Stat(dir); err == nil {
			add(dir)
		}
	}
	// 3. Retain existing PATH entries.
	for _, dir := range filepath.SplitList(current) {
		add(dir)
	}

	return strings.Join(result, string(os.PathListSeparator))
}

// moduleRoot returns the absolute path to the module root directory.
// This source file is at <module-root>/test/stress/stress_test.go,
// so we walk up two directories.
func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
