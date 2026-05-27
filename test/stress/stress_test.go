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

	"github.com/dmcgowan/shimtest"
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
// co-located binaries (nerdbox-kernel-*, nerdbox-initrd, libkrun.so) are
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
// Each subtest (Lifecycle, Exec, Transfer) runs until one minute before the
// -test.timeout deadline. Select individual subtests with -run:
//
//	-run TestShimStress/Lifecycle
//	-run TestShimStress/Exec
//	-run TestShimStress/Transfer
func TestShimStress(t *testing.T) {
	shimtest.NewStressSuite(shimConfig(), shimtest.StressOptions{
		Transfer: true,
	}).Run(t)
}

// shimPath returns a PATH value that prepends candidate _output directories
// to the current PATH. The local module _output/ is highest priority, followed
// by sibling worktree _output/ directories (to find kernel/initrd/libkrun built
// in another branch worktree).
func shimPath() string {
	root := moduleRoot()
	current := os.Getenv("PATH")

	var candidates []string
	candidates = append(candidates, filepath.Join(root, "_output"))

	// Walk sibling worktrees: the parent of root is the common worktree parent.
	parent := filepath.Dir(root)
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			if !e.IsDir() || e.Name() == filepath.Base(root) {
				continue
			}
			candidates = append(candidates, filepath.Join(parent, e.Name(), "_output"))
		}
	}

	// Build a set of existing PATH elements for exact membership tests.
	existing := make(map[string]bool)
	for _, e := range filepath.SplitList(current) {
		existing[e] = true
	}

	var prepend []string
	for _, dir := range candidates {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if existing[dir] {
			continue
		}
		prepend = append(prepend, dir)
	}
	if len(prepend) == 0 {
		return current
	}
	return strings.Join(prepend, string(os.PathListSeparator)) +
		string(os.PathListSeparator) + current
}

// moduleRoot returns the absolute path to the module root directory.
// This source file is at <module-root>/test/stress/stress_test.go,
// so we walk up two directories.
func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
