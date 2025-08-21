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
	"fmt"
	"os"
	"path/filepath"
	"sync"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/runc"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	bundleAPI "github.com/dmcgowan/nerdbox/api/services/bundle/v1"
	"github.com/dmcgowan/nerdbox/internal/vm"
)

var (
	_     = shim.TTRPCService(&service{})
	empty = &ptypes.Empty{}
)

// NewTaskService creates a new instance of a task service
func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	s := &service{
		context: ctx,
		events:  make(chan interface{}, 128),
		//ec:      reaper.Default.Subscribe(),
		shutdown: sd,
		//containers:           make(map[string]*runc.Container),
		//running:              make(map[int][]containerProcess),
		//runningExecs:         make(map[*runc.Container]int),
		//execCountSubscribers: make(map[*runc.Container]chan<- int),
		//containerInitExit:    make(map[*runc.Container]runcC.Exit),
		//exitSubscribers:      make(map[*map[int][]runcC.Exit]struct{}),
	}
	/*
		go s.processExits()
		runcC.Monitor = reaper.Default
		if err := s.initPlatform(); err != nil {
			return nil, fmt.Errorf("failed to initialized platform behavior: %w", err)
		}
		go s.forward(ctx, publisher)
	*/
	sd.RegisterCallback(func(context.Context) error {
		close(s.events)
		return nil
	})

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			return shim.RemoveSocket(address)
		})
	}
	return s, nil
}

type vmProcess struct {
	pid    int
	path   string
	client *ttrpc.Client
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	mu sync.Mutex
	vm vm.Instance

	context  context.Context
	events   chan interface{}
	platform stdio.Platform

	//ec       chan runcC.Exit
	//ep       oom.Watcher

	//containers map[string]*runc.Container

	//lifecycleMu  sync.Mutex
	//running      map[int][]containerProcess // pid -> running process, guarded by lifecycleMu
	//runningExecs map[*runc.Container]int    // container -> num running execs, guarded by lifecycleMu
	// container -> subscription to exec exits/changes to s.runningExecs[container],
	// guarded by lifecycleMu
	//execCountSubscribers map[*runc.Container]chan<- int
	// container -> init exits, guarded by lifecycleMu
	// Used to stash container init process exits, so that we can hold them
	// until after we've made sure to publish all the container's exec exits.
	// Also used to prevent starting new execs from being started if the
	// container's init process (read: pid, not [process.Init]) has already been
	// reaped by the shim.
	// Note that this flag gets updated before the container's [process.Init.Status]
	// is transitioned to "stopped".
	//containerInitExit map[*runc.Container]runcC.Exit
	// Subscriptions to exits for PIDs. Adding/deleting subscriptions and
	// dereferencing the subscription pointers must only be done while holding
	// lifecycleMu.
	//exitSubscribers map[*map[int][]runcC.Exit]struct{}

	shutdown shutdown.Service
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

type containerProcess struct {
	Container *runc.Container
	Process   process.Process
}

// Create a new initial process and container with the underlying OCI runtime
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	log.G(ctx).WithFields(log.Fields{
		"id":     r.ID,
		"bundle": r.Bundle,
		"rootfs": r.Rootfs,
		"stdin":  r.Stdin,
		"stdout": r.Stdout,
		"stderr": r.Stderr,
	}).Info("creating container task")

	if r.Checkpoint != "" || r.ParentCheckpoint != "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("checkpoints not supported: %w", errdefs.ErrNotImplemented))
	}

	// Handle mounts
	m, err := setupMounts(r.ID, r.Rootfs, filepath.Join(r.Bundle, "rootfs"))
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// TODO: Consider delaying VM start to task start, this could allow
	// the VM to be started with the resources from all container creates

	mounts := map[string]string{
		fmt.Sprintf("rootfs-%s", r.ID): filepath.Join(r.Bundle, "rootfs"),
	}
	if err := s.startVM(ctx, filepath.Join(r.Bundle, "run_vminitd.sock"), mounts); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	s.shutdown.RegisterCallback(s.vm.Shutdown)

	cb, err := os.ReadFile(filepath.Join(r.Bundle, "config.json"))
	if err != nil {
		return nil, errgrpc.ToGRPCf(err, "failed to read config file")
	}

	br, err := bundleAPI.NewTTRPCBundleClient(s.vm.Client()).Create(ctx, &bundleAPI.CreateRequest{
		ID:     r.ID,
		Config: cb,
	})
	if err != nil {
		return nil, err
	}

	vr := &taskAPI.CreateTaskRequest{
		ID:     r.ID,
		Bundle: br.Bundle,
		Rootfs: m,
		// TODO: Add terminal and stdio
		// Terminal: r.Terminal,
		// Stdin:   r.Stdin,
		// Stdout:  r.Stdout,
		// Stderr:  r.Stderr,
		Options: r.Options,
	}

	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	resp, err := tc.Create(ctx, vr)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to create task")
		return nil, errgrpc.ToGRPC(err)
	} else {
		log.G(ctx).Debug("no failure creating task")
	}

	// TODO: Keep track of container until successfully removed

	// TODO: Forward events rather than generate here?
	s.send(&eventstypes.TaskCreate{
		ContainerID: r.ID,
		Bundle:      r.Bundle,
		Rootfs:      r.Rootfs,
		IO: &eventstypes.TaskIO{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		Pid: resp.Pid,
	})

	// The following line cannot return an error as the only state in which that
	// could happen would also cause the container.Pid() call above to
	// nil-deference panic.
	//proc, _ := container.Process("")
	//handleStarted(container, proc)

	return &taskAPI.CreateTaskResponse{
		Pid: resp.Pid,
	}, nil
}

// Start a process
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("starting container task")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Start(ctx, r)
}

// Delete the initial process and container
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("deleting container")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Delete(ctx, r)
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("exec container")

	vr := &taskAPI.ExecProcessRequest{
		ID:     r.ID,
		ExecID: r.ExecID,
		// TODO: Enable support for terminal and stdio
		//Terminal: r.Terminal,
		//Stdin:    r.Stdin,
		//Stdout:   r.Stdout,
		//Stderr:   r.Stderr,
		Spec: r.Spec,
	}
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Exec(ctx, vr)
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("resize pty")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.ResizePty(ctx, r)
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	st, err := tc.State(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("state")
		return nil, err
	}
	log.G(ctx).WithFields(log.Fields{"status": st.Status, "id": r.ID, "exec": r.ExecID}).Info("state")

	return st, err
}

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("pause")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Pause(ctx, r)
}

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("resume")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Resume(ctx, r)
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("kill")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Kill(ctx, r)
}

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("all pids")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Pids(ctx, r)
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("close io")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.CloseIO(ctx, r)
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("checkpoint")
	/*
		container, err := s.getContainer(r.ID)
		if err != nil {
			return nil, err
		}
		if err := container.Checkpoint(ctx, r); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
	*/
	return empty, nil
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("update")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Update(ctx, r)
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("wait")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Wait(ctx, r)
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("connect")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	vr, err := tc.Connect(ctx, r)
	if err != nil {
		return nil, err
	}

	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: vr.TaskPid,
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("shutdown")
	//tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	//return tc.Shutdown(ctx, r)

	// TODO: Keep track of running containers (or rely on sandbox interface?)
	/*
		s.mu.Lock()
		defer s.mu.Unlock()

		// return out if the shim is still servicing containers
		if len(s.containers) > 0 {
			return empty, nil
		}
	*/

	// please make sure that temporary resource has been cleanup or registered
	// for cleanup before calling shutdown
	s.shutdown.Shutdown()

	return empty, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("stats")
	tc := taskAPI.NewTTRPCTaskClient(s.vm.Client())
	return tc.Stats(ctx, r)
}

/*
func (s *service) processExits() {
	for e := range s.ec {
		// While unlikely, it is not impossible for a container process to exit
		// and have its PID be recycled for a new container process before we
		// have a chance to process the first exit. As we have no way to tell
		// for sure which of the processes the exit event corresponds to (until
		// pidfd support is implemented) there is no way for us to handle the
		// exit correctly in that case.

		s.lifecycleMu.Lock()
		// Inform any concurrent s.Start() calls so they can handle the exit
		// if the PID belongs to them.
		for subscriber := range s.exitSubscribers {
			(*subscriber)[e.Pid] = append((*subscriber)[e.Pid], e)
		}
		// Handle the exit for a created/started process. If there's more than
		// one, assume they've all exited. One of them will be the correct
		// process.
		var cps []containerProcess
		for _, cp := range s.running[e.Pid] {
			_, init := cp.Process.(*process.Init)
			if init {
				s.containerInitExit[cp.Container] = e
			}
			cps = append(cps, cp)
		}
		delete(s.running, e.Pid)
		s.lifecycleMu.Unlock()

		for _, cp := range cps {
			if ip, ok := cp.Process.(*process.Init); ok {
				s.handleInitExit(e, cp.Container, ip)
			} else {
				s.handleProcessExit(e, cp.Container, cp.Process)
			}
		}
	}
}
*/

func (s *service) send(evt interface{}) {
	s.events <- evt
}

/*
// handleInitExit processes container init process exits.
// This is handled separately from non-init exits, because there
// are some extra invariants we want to ensure in this case, namely:
// - for a given container, the init process exit MUST be the last exit published
// This is achieved by:
// - killing all running container processes (if the container has a shared pid
// namespace, otherwise all other processes have been reaped already).
// - waiting for the container's running exec counter to reach 0.
// - finally, publishing the init exit.
func (s *service) handleInitExit(e runcC.Exit, c *runc.Container, p *process.Init) {
	// kill all running container processes
	if runc.ShouldKillAllOnExit(s.context, c.Bundle) {
		if err := p.KillAll(s.context); err != nil {
			log.G(s.context).WithError(err).WithField("id", p.ID()).
				Error("failed to kill init's children")
		}
	}

	s.lifecycleMu.Lock()
	numRunningExecs := s.runningExecs[c]
	if numRunningExecs == 0 {
		delete(s.runningExecs, c)
		s.lifecycleMu.Unlock()
		s.handleProcessExit(e, c, p)
		return
	}

	events := make(chan int, numRunningExecs)
	s.execCountSubscribers[c] = events

	s.lifecycleMu.Unlock()

	go func() {
		defer func() {
			s.lifecycleMu.Lock()
			defer s.lifecycleMu.Unlock()
			delete(s.execCountSubscribers, c)
			delete(s.runningExecs, c)
		}()

		// wait for running processes to exit
		for {
			if runningExecs := <-events; runningExecs == 0 {
				break
			}
		}

		// all running processes have exited now, and no new
		// ones can start, so we can publish the init exit
		s.handleProcessExit(e, c, p)
	}()
}

func (s *service) handleProcessExit(e runcC.Exit, c *runc.Container, p process.Process) {
	p.SetExited(e.Status)
	s.send(&eventstypes.TaskExit{
		ContainerID: c.ID,
		ID:          p.ID(),
		Pid:         uint32(e.Pid),
		ExitStatus:  uint32(e.Status),
		ExitedAt:    protobuf.ToTimestamp(p.ExitedAt()),
	})
	if _, init := p.(*process.Init); !init {
		s.lifecycleMu.Lock()
		s.runningExecs[c]--
		if ch, ok := s.execCountSubscribers[c]; ok {
			ch <- s.runningExecs[c]
		}
		s.lifecycleMu.Unlock()
	}
}

func (s *service) getContainerPids(ctx context.Context, container *runc.Container) ([]uint32, error) {
	p, err := container.Process("")
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	ps, err := p.(*process.Init).Runtime().Ps(ctx, container.ID)
	if err != nil {
		return nil, err
	}
	pids := make([]uint32, 0, len(ps))
	for _, pid := range ps {
		pids = append(pids, uint32(pid))
	}
	return pids, nil
}

func (s *service) forward(ctx context.Context, publisher shim.Publisher) {
	ns, _ := namespaces.Namespace(ctx)
	ctx = namespaces.WithNamespace(context.Background(), ns)
	for e := range s.events {
		err := publisher.Publish(ctx, runtime.GetTopic(e), e)
		if err != nil {
			log.G(ctx).WithError(err).Error("post event")
		}
	}
	publisher.Close()
}

func (s *service) getContainer(id string) (*runc.Container, error) {
	s.mu.Lock()
	container := s.containers[id]
	s.mu.Unlock()
	if container == nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "container not created")
	}
	return container, nil
}

// initialize a single epoll fd to manage our consoles. `initPlatform` should
// only be called once.
func (s *service) initPlatform() error {
	if s.platform != nil {
		return nil
	}
	p, err := runc.NewPlatform()
	if err != nil {
		return err
	}
	s.platform = p
	s.shutdown.RegisterCallback(func(context.Context) error { return s.platform.Close() })
	return nil
}
*/
