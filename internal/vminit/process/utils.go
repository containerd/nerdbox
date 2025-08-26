//go:build !windows

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

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	runc "github.com/containerd/go-runc"
	"golang.org/x/sys/unix"

	"github.com/dmcgowan/nerdbox/internal/vminit/stream"
)

const (
	// RuncRoot is the path to the root runc state directory
	RuncRoot = "/run/runc"
	// InitPidFile name of the file that contains the init pid
	InitPidFile = "init.pid"
)

// safePid is a thread safe wrapper for pid.
type safePid struct {
	sync.Mutex
	pid int
}

func (s *safePid) get() int {
	s.Lock()
	defer s.Unlock()
	return s.pid
}

// TODO(mlaventure): move to runc package?
func getLastRuntimeError(r *runc.Runc) (string, error) {
	if r.Log == "" {
		return "", nil
	}

	f, err := os.OpenFile(r.Log, os.O_RDONLY, 0400)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var (
		errMsg string
		log    struct {
			Level string
			Msg   string
			Time  time.Time
		}
	)

	dec := json.NewDecoder(f)
	for err = nil; err == nil; {
		if err = dec.Decode(&log); err != nil && err != io.EOF {
			return "", err
		}
		if log.Level == "error" {
			errMsg = strings.TrimSpace(log.Msg)
		}
	}

	return errMsg, nil
}

// criuError returns only the first line of the error message from criu
// it tries to add an invalid dump log location when returning the message
func criuError(err error) string {
	parts := strings.Split(err.Error(), "\n")
	return parts[0]
}

func copyFile(to, from string) error {
	ff, err := os.Open(from)
	if err != nil {
		return err
	}
	defer ff.Close()
	tt, err := os.Create(to)
	if err != nil {
		return err
	}
	defer tt.Close()

	p := bufPool.Get().(*[]byte)
	defer bufPool.Put(p)
	_, err = io.CopyBuffer(tt, ff, *p)
	return err
}

func checkKillError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "os: process already finished") ||
		strings.Contains(err.Error(), "container not running") ||
		strings.Contains(strings.ToLower(err.Error()), "no such process") ||
		err == unix.ESRCH {
		return fmt.Errorf("process already finished: %w", errdefs.ErrNotFound)
	} else if strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("no such container: %w", errdefs.ErrNotFound)
	}
	return fmt.Errorf("unknown error after kill: %w", err)
}

func newPidFile(bundle string) *pidFile {
	return &pidFile{
		path: filepath.Join(bundle, InitPidFile),
	}
}

func newExecPidFile(bundle, id string) *pidFile {
	return &pidFile{
		path: filepath.Join(bundle, fmt.Sprintf("%s.pid", id)),
	}
}

type pidFile struct {
	path string
}

func (p *pidFile) Path() string {
	return p.path
}

func (p *pidFile) Read() (int, error) {
	return runc.ReadPidFile(p.path)
}

// waitTimeout handles waiting on a waitgroup with a specified timeout.
// this is commonly used for waiting on IO to finish after a process has exited
func waitTimeout(ctx context.Context, wg *sync.WaitGroup, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func stateName(v interface{}) string {
	switch v.(type) {
	case *runningState, *execRunningState:
		return "running"
	case *createdState, *execCreatedState, *createdCheckpointState:
		return "created"
	case *pausedState:
		return "paused"
	case *deletedState:
		return "deleted"
	case *stoppedState:
		return "stopped"
	}
	panic(fmt.Errorf("invalid state %v", v))
}

func getStreams(stdio stdio.Stdio, sm stream.Manager) ([3]io.ReadWriteCloser, error) {
	var streams [3]io.ReadWriteCloser
	var err error
	if stdio.Stdin != "" {
		streams[0], err = getStream(stdio.Stdin, sm)
		if err != nil {
			return streams, fmt.Errorf("failed to get stdin stream: %w", err)
		}
	}
	if stdio.Stdout != "" {
		streams[1], err = getStream(stdio.Stdout, sm)
		if err != nil {
			if streams[0] != nil {
				streams[0].Close()
			}
			return streams, fmt.Errorf("failed to get stdout stream: %w", err)
		}
	}
	if stdio.Stderr != "" {
		streams[2], err = getStream(stdio.Stderr, sm)
		if err != nil {
			if streams[0] != nil {
				streams[0].Close()
			}
			if streams[1] != nil {
				streams[1].Close()
			}
			return streams, fmt.Errorf("failed to get stderr stream: %w", err)
		}
	}
	return streams, nil
}

func getStream(uri string, sm stream.Manager) (io.ReadWriteCloser, error) {
	if !strings.HasPrefix(uri, "stream://") {
		return nil, fmt.Errorf("not a stream: %s", errdefs.ErrInvalidArgument)
	}
	sid, err := strconv.ParseUint(uri[9:], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid stream id %q: %w", uri, err)
	}
	c, err := sm.Get(uint32(sid))
	if err != nil {
		return nil, fmt.Errorf("unable to get stream %d: %w", sid, errdefs.ErrNotFound)
	}
	return c, err
}
