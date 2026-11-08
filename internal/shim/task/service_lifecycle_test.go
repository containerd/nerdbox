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
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/nerdbox/internal/shim/sandbox"
	vmsandbox "github.com/containerd/nerdbox/internal/shim/sandbox/vm"
	vmapi "github.com/containerd/nerdbox/pkg/vm"
	"github.com/containerd/ttrpc"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

func TestCreateDuringShutdownReturnsUnavailable(t *testing.T) {
	bundleDir := t.TempDir()
	rootfs := filepath.Join(bundleDir, "rootfs")
	if err := os.Mkdir(rootfs, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(ocispec.Spec{
		Version: ocispec.Version,
		Root:    &ocispec.Root{Path: "rootfs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}

	sd := newControlledShutdown()
	manager := &lifecycleManager{instance: &lifecycleInstance{}}
	sbx := vmsandbox.NewVMSandbox(manager)
	if err := sbx.Start(t.Context(), sandbox.WithStateDir(t.TempDir())); err != nil {
		t.Fatalf("starting initial VM instance: %v", err)
	}
	svc, err := NewTaskService(t.Context(), &clientlessSandbox{Sandbox: sbx}, discardPublisher{}, sd)
	if err != nil {
		t.Fatal(err)
	}

	shutdownDone := make(chan error, 1)
	go func() {
		_, err := svc.Shutdown(t.Context(), &taskAPI.ShutdownRequest{ID: "test"})
		shutdownDone <- err
	}()
	<-sd.started

	_, err = svc.Create(t.Context(), &taskAPI.CreateTaskRequest{
		ID:     "test",
		Bundle: bundleDir,
	})
	if !errdefs.IsUnavailable(errgrpc.ToNative(err)) {
		t.Fatalf("Create during shutdown error = %v, want unavailable", err)
	}
	if got := manager.calls(); got != 1 {
		t.Fatalf("VM instance created %d times, want only the initial instance", got)
	}

	close(sd.release)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

func TestShutdownReturnsSandboxStopError(t *testing.T) {
	stopErr := errors.New("VM shutdown failed")
	sd := newControlledShutdown()
	manager := &lifecycleManager{instance: &lifecycleInstance{shutdownErr: stopErr}}
	sbx := vmsandbox.NewVMSandbox(manager)
	if err := sbx.Start(t.Context(), sandbox.WithStateDir(t.TempDir())); err != nil {
		t.Fatalf("starting initial VM instance: %v", err)
	}
	svc, err := NewTaskService(t.Context(), &clientlessSandbox{Sandbox: sbx}, discardPublisher{}, sd)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.Shutdown(t.Context(), &taskAPI.ShutdownRequest{ID: "test"})
		done <- err
	}()
	<-sd.started
	close(sd.release)

	err = errgrpc.ToNative(<-done)
	if err == nil || !strings.Contains(err.Error(), stopErr.Error()) {
		t.Fatalf("Shutdown error = %v, want %v", err, stopErr)
	}
}

func TestCreateDuringDirectShutdownReturnsUnavailable(t *testing.T) {
	bundleDir := t.TempDir()
	rootfs := filepath.Join(bundleDir, "rootfs")
	if err := os.Mkdir(rootfs, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(ocispec.Spec{
		Version: ocispec.Version,
		Root:    &ocispec.Root{Path: "rootfs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}

	sd := newControlledShutdown()
	instance := &lifecycleInstance{
		shutdownStarted: make(chan struct{}),
		releaseShutdown: make(chan struct{}),
	}
	manager := &lifecycleManager{instance: instance}
	sbx := vmsandbox.NewVMSandbox(manager)
	if err := sbx.Start(t.Context(), sandbox.WithStateDir(t.TempDir())); err != nil {
		t.Fatalf("starting initial VM instance: %v", err)
	}
	svc, err := NewTaskService(t.Context(), &clientlessSandbox{Sandbox: sbx}, discardPublisher{}, sd)
	if err != nil {
		t.Fatal(err)
	}

	sd.Shutdown()
	<-sd.started
	close(sd.release)
	<-instance.shutdownStarted

	_, err = svc.Create(t.Context(), &taskAPI.CreateTaskRequest{ID: "test", Bundle: bundleDir})
	if !errdefs.IsUnavailable(errgrpc.ToNative(err)) {
		t.Fatalf("Create during direct shutdown error = %v, want unavailable", err)
	}
	if got := manager.calls(); got != 1 {
		t.Fatalf("VM instance created %d times, want only the initial instance", got)
	}

	close(instance.releaseShutdown)
	<-sd.done
}

func TestInitialDeleteDoesNotRetireService(t *testing.T) {
	svc, err := NewTaskService(t.Context(), &clientlessSandbox{}, discardPublisher{}, newControlledShutdown())
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Delete(t.Context(), &taskAPI.DeleteRequest{ID: "test"})
	if err == nil {
		t.Fatal("Delete returned nil error without a VM client")
	}

	service := svc.(*service)
	service.lifecycleMu.Lock()
	defer service.lifecycleMu.Unlock()
	if service.retiring {
		t.Fatal("initial Delete retired the sandbox service")
	}
}

type controlledShutdown struct {
	mu        sync.Mutex
	callbacks []func(context.Context) error
	started   chan struct{}
	release   chan struct{}
	done      chan struct{}
	err       error
	once      sync.Once
}

func newControlledShutdown() *controlledShutdown {
	return &controlledShutdown{
		started: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (s *controlledShutdown) Shutdown() {
	s.once.Do(func() {
		close(s.started)
		go func() {
			<-s.release
			s.mu.Lock()
			callbacks := append([]func(context.Context) error(nil), s.callbacks...)
			s.mu.Unlock()
			var shutdownErr error
			for _, callback := range callbacks {
				if err := callback(context.Background()); err != nil && shutdownErr == nil {
					shutdownErr = err
				}
			}
			if shutdownErr == nil {
				shutdownErr = shutdown.ErrShutdown
			}
			s.mu.Lock()
			s.err = shutdownErr
			s.mu.Unlock()
			close(s.done)
		}()
	})
}

func (s *controlledShutdown) RegisterCallback(callback func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callbacks = append(s.callbacks, callback)
}

func (s *controlledShutdown) Done() <-chan struct{} { return s.done }

func (s *controlledShutdown) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

type clientlessSandbox struct {
	sandbox.Sandbox
}

func (*clientlessSandbox) Client() (*ttrpc.Client, error) {
	return nil, errors.New("no VM client")
}

type lifecycleManager struct {
	vmapi.Manager

	mu       sync.Mutex
	instance vmapi.Instance
	newCalls int
}

func (m *lifecycleManager) NewInstance(context.Context, string) (vmapi.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.newCalls++
	return m.instance, nil
}

func (m *lifecycleManager) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newCalls
}

type lifecycleInstance struct {
	vmapi.Instance
	shutdownErr     error
	shutdownStarted chan struct{}
	releaseShutdown chan struct{}
}

func (*lifecycleInstance) Start(context.Context, ...vmapi.StartOpt) error { return nil }
func (i *lifecycleInstance) Shutdown(context.Context) error {
	if i.shutdownStarted != nil {
		close(i.shutdownStarted)
		<-i.releaseShutdown
	}
	return i.shutdownErr
}

type discardPublisher struct{}

func (discardPublisher) Publish(context.Context, string, events.Event) error { return nil }
func (discardPublisher) Close() error                                        { return nil }

var _ io.Closer = discardPublisher{}
