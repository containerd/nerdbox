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

package vm

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/containerd/ttrpc"

	"github.com/containerd/nerdbox/pkg/vm"
)

// blockingInstance is a fake vm.Instance whose StartStream and Shutdown
// block until the test releases them, simulating a guest that never acks
// the stream handshake, or a shutdown that hangs on an unresponsive guest.
type blockingInstance struct {
	vm.Instance
	startStreamCalled chan struct{}
	shutdownCalled    chan struct{}
	release           chan struct{}
	releaseOnce       sync.Once
}

// closeRelease is registered via t.Cleanup so a t.Fatal before the
// normal close doesn't leave a goroutine parked on b.release forever,
// hanging the rest of the test run.
func (b *blockingInstance) closeRelease() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingInstance) StartStream(ctx context.Context, streamID string, opts ...vm.StreamOpt) (net.Conn, error) {
	close(b.startStreamCalled)
	<-b.release
	return nil, nil
}

func (b *blockingInstance) Shutdown(ctx context.Context) error {
	close(b.shutdownCalled)
	<-b.release
	return nil
}

func (b *blockingInstance) Client() *ttrpc.Client {
	return &ttrpc.Client{}
}

func TestStartStreamDoesNotBlockClient(t *testing.T) {
	inst := &blockingInstance{
		startStreamCalled: make(chan struct{}),
		release:           make(chan struct{}),
	}
	t.Cleanup(inst.closeRelease)
	s := &localsandbox{instance: inst}

	done := make(chan error, 1)
	go func() {
		_, err := s.StartStream(context.Background(), "test-stream")
		done <- err
	}()

	select {
	case <-inst.startStreamCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("StartStream was never called on the instance")
	}

	clientDone := make(chan struct{})
	go func() {
		if _, err := s.Client(); err != nil {
			t.Errorf("Client() returned error: %v", err)
		}
		close(clientDone)
	}()

	select {
	case <-clientDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Client() blocked behind an in-flight StartStream handshake")
	}

	inst.closeRelease()
	if err := <-done; err != nil {
		t.Fatalf("StartStream returned error: %v", err)
	}
}

func TestStopDoesNotBlockClient(t *testing.T) {
	inst := &blockingInstance{
		shutdownCalled: make(chan struct{}),
		release:        make(chan struct{}),
	}
	t.Cleanup(inst.closeRelease)
	s := &localsandbox{instance: inst}

	done := make(chan error, 1)
	go func() {
		done <- s.Stop(context.Background())
	}()

	select {
	case <-inst.shutdownCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown was never called on the instance")
	}

	clientDone := make(chan struct{})
	go func() {
		if _, err := s.Client(); err == nil {
			t.Error("Client() should fail fast once Stop has begun, not succeed")
		}
		close(clientDone)
	}()

	select {
	case <-clientDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Client() blocked behind an in-flight Shutdown")
	}

	inst.closeRelease()
	if err := <-done; err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	if _, err := s.Client(); err == nil {
		t.Fatal("Client() should fail after Stop clears the instance")
	}
}

// TestConcurrentStopFailsFast verifies that a second Stop call made while
// the first is still in Shutdown fails immediately with a precondition
// error instead of racing into a second Shutdown call on the same
// instance.
func TestConcurrentStopFailsFast(t *testing.T) {
	inst := &blockingInstance{
		shutdownCalled: make(chan struct{}),
		release:        make(chan struct{}),
	}
	t.Cleanup(inst.closeRelease)
	s := &localsandbox{instance: inst}

	done := make(chan error, 1)
	go func() {
		done <- s.Stop(context.Background())
	}()

	select {
	case <-inst.shutdownCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown was never called on the instance")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- s.Stop(context.Background())
	}()

	select {
	case err := <-secondDone:
		if err == nil {
			t.Fatal("second concurrent Stop() should have failed instead of racing into Shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second concurrent Stop() blocked instead of failing fast")
	}

	inst.closeRelease()
	if err := <-done; err != nil {
		t.Fatalf("first Stop returned error: %v", err)
	}
}

// TestStartStreamFailsFastWhileStopping verifies that StartStream rejects
// new streams once Stop has begun tearing down the instance, instead of
// racing a new stream handshake against a concurrent Shutdown.
func TestStartStreamFailsFastWhileStopping(t *testing.T) {
	inst := &blockingInstance{
		shutdownCalled: make(chan struct{}),
		release:        make(chan struct{}),
	}
	t.Cleanup(inst.closeRelease)
	s := &localsandbox{instance: inst}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- s.Stop(context.Background())
	}()

	select {
	case <-inst.shutdownCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown was never called on the instance")
	}

	if _, err := s.StartStream(context.Background(), "test-stream"); err == nil {
		t.Fatal("StartStream() should fail fast once Stop has begun")
	}

	inst.closeRelease()
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
}
