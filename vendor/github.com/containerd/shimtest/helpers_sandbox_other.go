//go:build !linux

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

// Package shimtest provides sandbox helpers.  On non-Linux platforms the
// sandbox suite is not supported (virtiofs-backed shared container
// filesystems require Linux).  The helpers here are stubs that satisfy
// the compiler; the SandboxSuite.Run method skips the entire suite at
// runtime.

package shimtest

import (
	"context"
	"testing"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
)

type sandboxEnv struct{}

func startSandboxShim(_ testing.TB, _ Config, _ string) *sandboxEnv {
	return &sandboxEnv{}
}

func createContainerInSandbox(_ testing.TB, _ *sandboxEnv, _ []string, _ ...func(*sandboxCtrSpec)) (string, string) {
	return "", ""
}

type sandboxCtrSpec struct{}

func withSandboxCtrNoStart() func(*sandboxCtrSpec) { return func(*sandboxCtrSpec) {} }

func readSandboxOutput(_ testing.TB, _ context.Context, _, _ string, _ time.Duration) string {
	return ""
}

func sandboxShimPID(_ *sandboxEnv, _ string) int { return 0 }

func sandboxContainersMounts(_ int) []string { return nil }

func sandboxMountTargets(_ int) []string { return nil }

func waitForSandboxStatus(_ context.Context, _ sandboxAPI.TTRPCSandboxService, _, _ string, _ time.Duration) error {
	return nil
}

func shutdownSandboxShim(_ testing.TB, _ *sandboxEnv) {}
