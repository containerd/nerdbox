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
//	-run TestShim/Sandbox
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
//
// SandboxSuite verifies the containerd sandbox shim API contract
// (runtime/sandbox/v1): lifecycle, platform, ping, single and multiple
// member containers, per-container independence, and error cases.
func TestShim(t *testing.T) {
	cfg := shimConfig()
	shimtest.NewRunSuite(cfg).Run(t)
	shimtest.NewExecSuite(cfg).Run(t)
	shimtest.NewTransferSuite(cfg).Run(t)
	shimtest.NewOOMSuite(cfg).Run(t)
	shimtest.NewUDSSuite(cfg).Run(t)
	shimtest.NewLayersSuite(cfg).Run(t)
	shimtest.NewNetworkSuite(cfg).Run(t)
	shimtest.NewSandboxSuite(cfg).Run(t)
}

// BenchmarkShim runs the shimtest benchmark suites against the nerdbox shim.
// Run individual benchmarks with -bench, e.g.:
//
//	go test -bench 'BenchmarkShim/Lifecycle' -benchtime 5x ./test/shim/...
//	go test -bench 'BenchmarkShim/ContainerCreate' -benchtime 5x ./test/shim/...
//
// ContainerCreate benchmarks the per-container create/start/wait/delete cycle
// inside a shared sandbox VM; Lifecycle benchmarks the full shim-start +
// single-container cycle.  Comparing their ms/create and ms/total metrics
// shows the marginal cost of adding a container to an existing sandbox versus
// booting a fresh VM.
func BenchmarkShim(b *testing.B) {
	cfg := shimConfig()
	shimtest.NewRunSuite(cfg).Bench(b)
	shimtest.NewExecSuite(cfg).Bench(b)
	shimtest.NewLayersSuite(cfg).Bench(b)
	shimtest.NewSandboxSuite(cfg).Bench(b)
}

// FuzzTransferMissing exercises the transfer service with arbitrary paths
// that don't exist in the container. It verifies the shim returns an
// application-level error rather than hanging.
func FuzzTransferMissing(f *testing.F) {
	shimtest.NewTransferSuite(shimConfig()).Fuzz(f)
}

// shimPath returns a PATH value that prepends candidate _output directories
// to the current PATH. The local module _output/ is highest priority, followed
// by sibling worktree _output/ directories that do NOT contain a libkrun.so —
// those are included for kernel/rootfs/vminitd assets only.  Sibling _output
// dirs that carry a libkrun.so are skipped to prevent the shim from resolving
// a stale libkrun built in another worktree.
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
			// Skip sibling _output dirs that have their own libkrun.so;
			// using a stale libkrun can cause symbol-not-found crashes.
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
// This source file is at <module-root>/test/shim/shim_test.go,
// so we walk up two directories.
func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
