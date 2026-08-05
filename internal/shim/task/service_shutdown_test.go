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

package task

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/containerd/ttrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
)

// fakeSandbox is a sandbox.Sandbox whose Stop behaviour is controlled by the
// test. Client always fails, which makes service.shutdown skip the guest
// unmount phase and go straight to stopping the VM — the phase under test.
type fakeSandbox struct {
	// stopBlocks, when true, makes Stop block until released is closed,
	// ignoring its context. This models a vm.Instance implementation that
	// blocks on an internal handoff and cannot be cancelled.
	stopBlocks bool
	released   chan struct{}

	stopErr    error
	stopCalled chan struct{}
	// stopCtxErr records ctx.Err() as observed on entry to Stop, so a test can
	// assert Stop was not handed an already-dead context.
	stopCtxErr error
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{
		released:   make(chan struct{}),
		stopCalled: make(chan struct{}, 1),
	}
}

func (f *fakeSandbox) Start(context.Context, ...sandbox.Opt) error { return nil }

func (f *fakeSandbox) Stop(ctx context.Context) error {
	f.stopCtxErr = ctx.Err()
	select {
	case f.stopCalled <- struct{}{}:
	default:
	}
	if f.stopBlocks {
		// Deliberately ignores ctx.
		<-f.released
	}
	return f.stopErr
}

func (f *fakeSandbox) Client() (*ttrpc.Client, error) {
	return nil, errors.New("no VM client in test")
}

func (f *fakeSandbox) StartStream(context.Context, string) (net.Conn, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeSandbox) ReservedDisks() int { return 0 }

// newTestService builds a service with the given sandbox, wired up the way
// NewTaskService does but without the shim framework.
func newTestService(sb sandbox.Sandbox) *service {
	return &service{
		context:    context.Background(),
		sb:         sb,
		events:     make(chan any, 128),
		containers: make(map[string]*container),
	}
}

// TestShutdownReturnsWhenVMStopHangs is the regression test for the wedge:
// a VM that never finishes stopping must not hold shutdown open, because the
// steps after it are what let containerd delete the bundle. Before the fix,
// service.shutdown blocked in sb.Stop forever and never reached them.
func TestShutdownReturnsWhenVMStopHangs(t *testing.T) {
	sb := newFakeSandbox()
	sb.stopBlocks = true
	t.Cleanup(func() { close(sb.released) })

	s := newTestService(sb)

	// A deadline stands in for the shutdown service's overall budget.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.shutdown(ctx) }()

	select {
	case err := <-done:
		// The hang must be reported, not swallowed.
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sandbox shutdown")
	case <-time.After(5 * time.Second):
		t.Fatal("service.shutdown did not return while sb.Stop was blocked")
	}

	// Stop was actually attempted.
	select {
	case <-sb.stopCalled:
	default:
		t.Error("expected sb.Stop to have been called")
	}

	// The forwarder sentinel must still have been sent, so the event
	// forwarding goroutine can exit.
	select {
	case ev := <-s.events:
		assert.Nil(t, ev, "expected the nil shutdown sentinel")
	default:
		t.Error("shutdown sentinel was not sent when sb.Stop hung")
	}
}

// TestShutdownStopWaitEndsWithCallerContext pins where the VM stop's patience
// comes from: the caller's deadline, not a timeout of its own. It should wait
// out the context it was given, then stop waiting.
func TestShutdownStopWaitEndsWithCallerContext(t *testing.T) {
	sb := newFakeSandbox()
	sb.stopBlocks = true
	t.Cleanup(func() { close(sb.released) })

	s := newTestService(sb)

	const budget = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	err := s.shutdown(ctx)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.GreaterOrEqual(t, elapsed, budget,
		"the VM stop should wait out the caller's context, not give up early")
	// Generous upper bound: the point is that it stops waiting, not that it is
	// prompt to the millisecond.
	assert.Less(t, elapsed, budget+5*time.Second,
		"the VM stop should not outlive the caller's context")
}

// TestShutdownStopsVMWhenBudgetAlreadyExhausted covers the case where an
// earlier phase (a container IO shutdown waiting out the whole budget on a hung
// guest) leaves no time on the clock. Stopping the VM is what releases the
// bundle's disk files, so it must still be attempted with a live context rather
// than skipped — otherwise the VM survives and wedges the bundle, which is the
// failure the bounded shutdown exists to prevent.
func TestShutdownStopsVMWhenBudgetAlreadyExhausted(t *testing.T) {
	sb := newFakeSandbox()
	s := newTestService(sb)

	// An already-expired budget.
	ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()

	require.NoError(t, s.shutdown(ctx))

	select {
	case <-sb.stopCalled:
	default:
		t.Fatal("sb.Stop was not called when the shutdown budget was already spent")
	}
	assert.NoError(t, sb.stopCtxErr, "sb.Stop must not be handed an already-cancelled context")
}

func TestShutdownSucceedsWhenVMStops(t *testing.T) {
	sb := newFakeSandbox()
	s := newTestService(sb)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, s.shutdown(ctx))

	select {
	case ev := <-s.events:
		assert.Nil(t, ev)
	default:
		t.Error("shutdown sentinel was not sent")
	}
}

func TestShutdownPropagatesVMStopError(t *testing.T) {
	sb := newFakeSandbox()
	sb.stopErr = errors.New("boom")
	s := newTestService(sb)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := s.shutdown(ctx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "boom")
}

// TestShutdownSentinelDoesNotBlockOnFullChannel guards the other way shutdown
// used to be able to wedge: an unconditional send on a full event channel with
// no forwarder draining it.
func TestShutdownSentinelDoesNotBlockOnFullChannel(t *testing.T) {
	sb := newFakeSandbox()
	s := newTestService(sb)

	// Fill the event channel so the sentinel send cannot proceed.
	for len(s.events) < cap(s.events) {
		s.events <- struct{}{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.shutdown(ctx)
	}()

	select {
	case <-done:
	case <-time.After(eventSentinelTimeout + 3*time.Second):
		t.Fatal("service.shutdown blocked sending the sentinel on a full event channel")
	}
}

// TestShutdownWaitsIndefinitelyWithoutDeadline documents the consequence of
// deferring to the caller: given a context with no deadline, the VM stop waits.
// The shim framework always supplies one, so this is the contract, not a hazard.
func TestShutdownWaitsIndefinitelyWithoutDeadline(t *testing.T) {
	sb := newFakeSandbox()
	sb.stopBlocks = true

	s := newTestService(sb)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.shutdown(context.Background())
	}()

	select {
	case <-done:
		t.Fatal("shutdown returned while sb.Stop was still blocked and no deadline was set")
	case <-time.After(500 * time.Millisecond):
	}

	// Releasing Stop lets it finish, proving it was waiting on Stop and nothing else.
	close(sb.released)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return after sb.Stop was released")
	}
}
