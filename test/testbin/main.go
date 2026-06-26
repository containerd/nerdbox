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

// Testbin is a minimal multicall binary for use inside shimtest containers.
// It is always compiled for linux/GOARCH with CGO_ENABLED=0 and placed at
// test/shim/testdata/testbin and test/stress/testdata/testbin so that the
// shim and stress test suites can embed it in their container rootfs images.
//
// This main is intentionally a one-liner: all logic lives in the importable
// github.com/containerd/shimtest/testbin package so it is covered by
// go mod vendor and stays in sync with the vendored shimtest version.
package main

import "github.com/containerd/shimtest/testbin"

func main() { testbin.Main() }
