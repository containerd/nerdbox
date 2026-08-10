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

package shimtest

import (
	"context"
	"fmt"

	taskV2 "github.com/containerd/containerd/api/runtime/task/v2"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// taskClientForVersion selects the TTRPC task client for the API
// version a shim declared in its bootstrap params, matching
// containerd's own dispatch: a version 3 shim is dialed on
// containerd.task.v3.Task directly, a version 2 shim through the
// ttrpcV2Bridge below, which translates every v3 request/response
// pair to and from the v2 wire format.
//
// This mirrors the client-selection behavior of containerd's
// core/runtime/v2 NewTaskClient without importing that higher
// level package.
func taskClientForVersion(client *ttrpc.Client, version int) taskAPI.TTRPCTaskService {
	switch version {
	case 2:
		return &ttrpcV2Bridge{client: taskV2.NewTTRPCTaskClient(client)}
	case 3:
		return taskAPI.NewTTRPCTaskClient(client)
	default:
		// Unreachable: startShim validated the version at bootstrap.
		panic(fmt.Errorf("unsupported shim task API version %d", version))
	}
}

// ttrpcV2Bridge adapts a containerd.task.v2.Task TTRPC client to the
// taskAPI.TTRPCTaskService (v3) interface used throughout this repo,
// so callers don't need to special-case the shim's declared version.
type ttrpcV2Bridge struct {
	client taskV2.TTRPCTaskService
}

var _ taskAPI.TTRPCTaskService = (*ttrpcV2Bridge)(nil)

func (b *ttrpcV2Bridge) State(ctx context.Context, req *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	resp, err := b.client.State(ctx, &taskV2.StateRequest{
		ID:     req.GetID(),
		ExecID: req.GetExecID(),
	})
	return &taskAPI.StateResponse{
		ID:         resp.GetID(),
		Bundle:     resp.GetBundle(),
		Pid:        resp.GetPid(),
		Status:     resp.GetStatus(),
		Stdin:      resp.GetStdin(),
		Stdout:     resp.GetStdout(),
		Stderr:     resp.GetStderr(),
		Terminal:   resp.GetTerminal(),
		ExitStatus: resp.GetExitStatus(),
		ExitedAt:   resp.GetExitedAt(),
		ExecID:     resp.GetExecID(),
	}, err
}

func (b *ttrpcV2Bridge) Create(ctx context.Context, req *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	resp, err := b.client.Create(ctx, &taskV2.CreateTaskRequest{
		ID:               req.GetID(),
		Bundle:           req.GetBundle(),
		Rootfs:           req.GetRootfs(),
		Terminal:         req.GetTerminal(),
		Stdin:            req.GetStdin(),
		Stdout:           req.GetStdout(),
		Stderr:           req.GetStderr(),
		Checkpoint:       req.GetCheckpoint(),
		ParentCheckpoint: req.GetParentCheckpoint(),
		Options:          req.GetOptions(),
	})
	return &taskAPI.CreateTaskResponse{Pid: resp.GetPid()}, err
}

func (b *ttrpcV2Bridge) Start(ctx context.Context, req *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	resp, err := b.client.Start(ctx, &taskV2.StartRequest{
		ID:     req.GetID(),
		ExecID: req.GetExecID(),
	})
	return &taskAPI.StartResponse{Pid: resp.GetPid()}, err
}

func (b *ttrpcV2Bridge) Delete(ctx context.Context, req *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	resp, err := b.client.Delete(ctx, &taskV2.DeleteRequest{
		ID:     req.GetID(),
		ExecID: req.GetExecID(),
	})
	return &taskAPI.DeleteResponse{
		Pid:        resp.GetPid(),
		ExitStatus: resp.GetExitStatus(),
		ExitedAt:   resp.GetExitedAt(),
	}, err
}

func (b *ttrpcV2Bridge) Pids(ctx context.Context, req *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	resp, err := b.client.Pids(ctx, &taskV2.PidsRequest{ID: req.GetID()})
	return &taskAPI.PidsResponse{Processes: resp.GetProcesses()}, err
}

func (b *ttrpcV2Bridge) Pause(ctx context.Context, req *taskAPI.PauseRequest) (*emptypb.Empty, error) {
	return b.client.Pause(ctx, &taskV2.PauseRequest{ID: req.GetID()})
}

func (b *ttrpcV2Bridge) Resume(ctx context.Context, req *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	return b.client.Resume(ctx, &taskV2.ResumeRequest{ID: req.GetID()})
}

func (b *ttrpcV2Bridge) Checkpoint(ctx context.Context, req *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return b.client.Checkpoint(ctx, &taskV2.CheckpointTaskRequest{
		ID:      req.GetID(),
		Path:    req.GetPath(),
		Options: req.GetOptions(),
	})
}

func (b *ttrpcV2Bridge) Kill(ctx context.Context, req *taskAPI.KillRequest) (*emptypb.Empty, error) {
	return b.client.Kill(ctx, &taskV2.KillRequest{
		ID:     req.GetID(),
		ExecID: req.GetExecID(),
		Signal: req.GetSignal(),
		All:    req.GetAll(),
	})
}

func (b *ttrpcV2Bridge) Exec(ctx context.Context, req *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	return b.client.Exec(ctx, &taskV2.ExecProcessRequest{
		ID:       req.GetID(),
		ExecID:   req.GetExecID(),
		Terminal: req.GetTerminal(),
		Stdin:    req.GetStdin(),
		Stdout:   req.GetStdout(),
		Stderr:   req.GetStderr(),
		Spec:     req.GetSpec(),
	})
}

func (b *ttrpcV2Bridge) ResizePty(ctx context.Context, req *taskAPI.ResizePtyRequest) (*emptypb.Empty, error) {
	return b.client.ResizePty(ctx, &taskV2.ResizePtyRequest{
		ID:     req.GetID(),
		ExecID: req.GetExecID(),
		Width:  req.GetWidth(),
		Height: req.GetHeight(),
	})
}

func (b *ttrpcV2Bridge) CloseIO(ctx context.Context, req *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	return b.client.CloseIO(ctx, &taskV2.CloseIORequest{
		ID:     req.GetID(),
		ExecID: req.GetExecID(),
		Stdin:  req.GetStdin(),
	})
}

func (b *ttrpcV2Bridge) Update(ctx context.Context, req *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	return b.client.Update(ctx, &taskV2.UpdateTaskRequest{
		ID:          req.GetID(),
		Resources:   req.GetResources(),
		Annotations: req.GetAnnotations(),
	})
}

func (b *ttrpcV2Bridge) Wait(ctx context.Context, req *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	resp, err := b.client.Wait(ctx, &taskV2.WaitRequest{
		ID:     req.GetID(),
		ExecID: req.GetExecID(),
	})
	return &taskAPI.WaitResponse{
		ExitStatus: resp.GetExitStatus(),
		ExitedAt:   resp.GetExitedAt(),
	}, err
}

func (b *ttrpcV2Bridge) Stats(ctx context.Context, req *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	resp, err := b.client.Stats(ctx, &taskV2.StatsRequest{ID: req.GetID()})
	return &taskAPI.StatsResponse{Stats: resp.GetStats()}, err
}

func (b *ttrpcV2Bridge) Connect(ctx context.Context, req *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	resp, err := b.client.Connect(ctx, &taskV2.ConnectRequest{ID: req.GetID()})
	return &taskAPI.ConnectResponse{
		ShimPid: resp.GetShimPid(),
		TaskPid: resp.GetTaskPid(),
		Version: resp.GetVersion(),
	}, err
}

func (b *ttrpcV2Bridge) Shutdown(ctx context.Context, req *taskAPI.ShutdownRequest) (*emptypb.Empty, error) {
	return b.client.Shutdown(ctx, &taskV2.ShutdownRequest{
		ID:  req.GetID(),
		Now: req.GetNow(),
	})
}
