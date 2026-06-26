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

// Package shimtest is a conformance test suite for containerd shim
// implementations. The tests are organized into Suites — RunSuite,
// ExecSuite, TransferSuite, UDSSuite, OOMSuite — each gated against
// one feature key. Callers construct a Config and call the suite's
// Run method (or Bench / Fuzz) from their own test functions:
//
//	func TestMyShim(t *testing.T) {
//	    cfg := shimtest.Config{ShimBinary: "containerd-shim-myshim-v1"}
//	    shimtest.NewRunSuite(cfg).Run(t)
//	    shimtest.NewExecSuite(cfg).Run(t)
//	}
package shimtest

// shimtestNamespace is the containerd namespace used for all shimtest
// operations.
const shimtestNamespace = "shimtest"

// Config carries everything a suite needs to bring up a shim.
// Callers identify their shim and tune setup behavior via this
// struct; suites hold a copy and use it for every test they run.
//
// Config has no Skip field — choosing which suites to run is the
// caller's concern (skip a feature by not constructing the
// corresponding suite).
type Config struct {
	// ShimBinary is the name (resolved via PATH) or absolute path of
	// the shim binary under test.
	ShimBinary string

	// FormatMounts, when true, provides the rootfs to the shim as
	// formatted erofs/ext4 images plus a format/mkdir/overlay mount
	// descriptor — appropriate for VM-based shims that mount the
	// rootfs themselves. When false the rootfs is extracted (or
	// overlay-mounted on the host) and provided pre-mounted.
	FormatMounts bool

	// Env is a set of environment variables propagated to processes
	// spawned by the harness (the shim binary, runc, etc.). The local
	// JSON-driven runner applies this via os.Setenv before any test
	// runs; library callers can pre-populate the process environment
	// directly and leave this empty.
	Env map[string]string

	// Debug enables verbose logging from the shim.
	Debug bool
}
