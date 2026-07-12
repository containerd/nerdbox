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

package sandbox

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	"github.com/containerd/ttrpc"
)

// fakeSandbox is a minimal Sandbox for exercising SandboxService's own
// lifecycle state machine independent of any real VM. The zero value never
// fails; set stopErr to make Stop fail, for testing error propagation out
// of StopSandbox/ShutdownSandbox.
type fakeSandbox struct {
	stopErr error
}

func (fakeSandbox) Start(context.Context, ...Opt) error                   { return nil }
func (f fakeSandbox) Stop(context.Context) error                          { return f.stopErr }
func (fakeSandbox) Client() (*ttrpc.Client, error)                        { return nil, nil }
func (fakeSandbox) StartStream(context.Context, string) (net.Conn, error) { return nil, nil }
func (fakeSandbox) ReservedDisks() int                                    { return 0 }

// newTestSandboxService builds a SandboxService in the "created" state
// without going through CreateSandbox, which does real filesystem I/O
// (creating a "vm" state dir relative to the process's current directory).
// Setting the fields directly is safe here: this test file is in the same
// package.
func newTestSandboxService(t *testing.T, sb Sandbox) *SandboxService {
	t.Helper()
	sharedFS, err := NewSharedFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewSharedFS: %v", err)
	}
	s := NewSandboxService(sb)
	s.sandboxID = "test-sandbox"
	s.stateDir = t.TempDir()
	s.sharedFS = sharedFS
	s.networkSandbox = NoNetworkSandbox{}
	s.state = ""
	return s
}

// TestStartSandboxAfterStopSandbox verifies that StartSandbox rejects a
// sandbox that has already been stopped, rather than panicking on
// s.networkSandbox.Path() — StopSandbox sets networkSandbox to nil, and
// prior to this test's fix, StartSandbox only rejected the already-ready
// state, not the already-stopped one, so a restart attempt would
// dereference that nil.
func TestStartSandboxAfterStopSandbox(t *testing.T) {
	ctx := context.Background()
	s := newTestSandboxService(t, fakeSandbox{})

	if _, err := s.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{SandboxID: s.sandboxID}); err != nil {
		t.Fatalf("first StartSandbox: %v", err)
	}
	if _, err := s.StopSandbox(ctx, &sandboxAPI.StopSandboxRequest{SandboxID: s.sandboxID}); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}

	// This must return a clean error, not panic.
	if _, err := s.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{SandboxID: s.sandboxID}); err == nil {
		t.Fatal("StartSandbox after StopSandbox: got nil error, want one")
	}
}

// TestShutdownSandboxPropagatesStopError verifies that ShutdownSandbox
// surfaces a StopSandbox failure to the caller instead of unconditionally
// reporting success. A caller that only ever sees success has no signal to
// retry, so a failed VM shutdown would otherwise go unnoticed.
func TestShutdownSandboxPropagatesStopError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("vm stop failed")
	s := newTestSandboxService(t, fakeSandbox{stopErr: wantErr})

	if _, err := s.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{SandboxID: s.sandboxID}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}

	_, err := s.ShutdownSandbox(ctx, &sandboxAPI.ShutdownSandboxRequest{SandboxID: s.sandboxID})
	if err == nil {
		t.Fatal("ShutdownSandbox: got nil error, want one (Sandbox.Stop failed)")
	}
	if !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("ShutdownSandbox error = %v, want it to mention %q", err, wantErr)
	}
}
