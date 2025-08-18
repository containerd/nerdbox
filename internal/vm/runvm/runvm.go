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

package runvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/fifo"
	"github.com/containerd/ttrpc"

	"github.com/dmcgowan/nerdbox/internal/ttrpcutil"
	"github.com/dmcgowan/nerdbox/internal/vm"
)

func NewVMInstance() (vm.Instance, error) {
	// TODO: Get these from a configuration
	ep, err := exec.LookPath("run_vminitd")
	if err != nil {
		return nil, fmt.Errorf("failed to find run_vminitd in PATH: %w", err)
	}

	return &vmInstance{
		binary: ep,
	}, nil
}

type vmInstance struct {
	mu     sync.Mutex
	binary string

	shutdownCallbacks []func(context.Context) error // Callbacks for shutdown

	pid    int
	path   string
	client *ttrpc.Client
}

func (v *vmInstance) Start(ctx context.Context, socketPath string, mounts map[string]string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pid > 0 {
		return fmt.Errorf("VM instance already started with PID %d", v.pid)
	}

	if _, err := os.Stat(socketPath); err == nil {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			log.Fatal("Failed to remove old socket file:", err)
		}
		log.Println("Removed old socket file:", socketPath)
	} else if !os.IsNotExist(err) {
		log.Fatal(err)
	}

	cf := "./run_vminitd.fifo"
	lr, err := fifo.OpenFifo(ctx, cf, os.O_RDONLY|os.O_CREATE|syscall.O_NONBLOCK, 0644)
	if err != nil {
		log.Fatal(err)
	}
	// TODO: Close this on shutdown?

	args := []string{
		"-l", socketPath,
		"-c", cf,
		"-k", "console=hvc0",
	}
	go io.Copy(os.Stderr, lr)

	if len(mounts) > 1 {
		return fmt.Errorf("multiple mounts are not yet supported: %w", errdefs.ErrNotImplemented)
	}
	for tag, p := range mounts {
		args = append(args, "-v", fmt.Sprintf("%s=%s", tag, p))
	}

	cmd := exec.CommandContext(ctx, v.binary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	goruntime.LockOSThread()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start VM: %w", err)
	}
	goruntime.UnlockOSThread()

	errC := make(chan error)
	go func() {
		defer close(errC)
		if err := cmd.Wait(); err != nil {
			errC <- err
		}
	}()

	v.shutdownCallbacks = []func(context.Context) error{
		func(context.Context) error {
			cerr := cmd.Cancel()
			select {
			case err := <-errC:
				if err != nil {
					return fmt.Errorf("failure running vm: %w", err)
				}
			default:
			}
			return cerr
		},
	}

	var conn net.Conn
	d := 2 * time.Millisecond
	for {
		select {
		case err := <-errC:
			if err != nil {
				return fmt.Errorf("failure running vm: %w", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
		if _, err := os.Stat(socketPath); err == nil {
			conn, err = net.Dial("unix", socketPath)
			if err != nil {
				return fmt.Errorf("failed to connect to TTRPC server: %w", err)
			}
			if err := ttrpcutil.PingTTRPC(conn, d); err != nil {
				conn.Close()
				d = d + time.Millisecond
				continue
			}

			v.shutdownCallbacks = append(v.shutdownCallbacks, func(context.Context) error {
				return conn.Close()
			})
			break
		}
	}

	v.pid = cmd.Process.Pid
	v.path = socketPath
	v.client = ttrpc.NewClient(conn)
	return nil
}

func (v *vmInstance) Client() *ttrpc.Client {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.client
}

func lookupFile(file string) (string, error) {
	path := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			// Unix shell semantics: path element "" means "."
			dir = "."
		}
		path := filepath.Join(dir, file)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", os.ErrNotExist
}

func (v *vmInstance) Shutdown(ctx context.Context) error {
	var err error
	for _, c := range v.shutdownCallbacks {
		if c != nil {
			if cerr := c(ctx); cerr != nil {
				if err == nil {
					err = cerr
				} else {
					err = errors.Join(err, cerr)
				}
			}
		}
	}
	return err
}
