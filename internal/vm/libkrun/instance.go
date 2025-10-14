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

package libkrun

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/fifo"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/ebitengine/purego"

	"github.com/containerd/nerdbox/internal/ttrpcutil"
	"github.com/containerd/nerdbox/internal/vm"
)

const vmStartTimeout = 5 * time.Second

var setLogging sync.Once

func NewManager() vm.Manager {
	return &vmManager{}
}

type vmManager struct{}

func (*vmManager) NewInstance(ctx context.Context, state string) (vm.Instance, error) {
	var (
		p1         = filepath.SplitList(os.Getenv("PATH"))
		p2         = filepath.SplitList(os.Getenv("LIBKRUN_PATH"))
		krunPath   string
		kernelPath string
		initrdPath string
	)
	if len(p2) == 0 {
		p2 = []string{"/usr/local/lib", "/usr/lib", "/lib"}
	}
	sharedName := "libkrun.so"
	if runtime.GOOS == "darwin" {
		sharedName = "libkrun-efi.dylib"
		p2 = append(p2, "/opt/homebrew/lib")
	}

	for _, dir := range append(p1, p2...) {
		if dir == "" {
			// Unix shell semantics: path element "" means "."
			dir = "."
		}
		var path string
		if krunPath == "" {
			path = filepath.Join(dir, sharedName)
			if _, err := os.Stat(path); err == nil {
				krunPath = path
			}
		}
		if kernelPath == "" {
			path = filepath.Join(dir, fmt.Sprintf("nerdbox-kernel-%s", kernelArch()))
			if _, err := os.Stat(path); err == nil {
				kernelPath = path
			}
		}
		if initrdPath == "" {
			path = filepath.Join(dir, "nerdbox-initrd")
			if _, err := os.Stat(path); err == nil {
				initrdPath = path
			}
		}
	}
	if krunPath == "" {
		return nil, fmt.Errorf("%s not found in PATH or LIBKRUN_PATH", sharedName)
	}
	if kernelPath == "" {
		return nil, fmt.Errorf("nerdbox-kernel not found in PATH or LIBKRUN_PATH")
	}
	if initrdPath == "" {
		return nil, fmt.Errorf("nerdbox-initrd not found in PATH or LIBKRUN_PATH")
	}

	lib, handler, err := openLibkrun(krunPath)
	if err != nil {
		return nil, err
	}

	var ret int32
	setLogging.Do(func() {
		ret = lib.InitLog(os.Stderr.Fd(), uint32(debugLevel), 0, 0)
	})
	if ret != 0 {
		return nil, fmt.Errorf("krun_init_log failed: %d", ret)
	}

	vmc, err := newvmcontext(lib)
	if err != nil {
		return nil, err
	}

	return &vmInstance{
		vmc:        vmc,
		state:      state,
		kernelPath: kernelPath,
		initrdPath: initrdPath,
		streamPath: filepath.Join(state, "streaming.sock"),
		lib:        lib,
		handler:    handler,
	}, nil
}

type vmInstance struct {
	mu    sync.Mutex
	vmc   *vmcontext
	state string

	kernelPath string
	initrdPath string
	streamPath string

	streamC uint32

	lib     *libkrun
	handler uintptr

	client            *ttrpc.Client
	shutdownCallbacks []func(context.Context) error
}

func (v *vmInstance) AddFS(ctx context.Context, tag, mountPath string, opts ...vm.MountOpt) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// TODO: Cannot be started?

	if err := v.vmc.AddVirtiofs(tag, mountPath); err != nil {
		return fmt.Errorf("failed to add virtiofs: %w", err)
	}

	return nil
}

func (v *vmInstance) AddDisk(ctx context.Context, blockID, mountPath string, opts ...vm.MountOpt) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	var mc vm.MountConfig
	for _, o := range opts {
		o(&mc)
	}

	if err := v.vmc.AddDisk(blockID, mountPath, mc.Readonly); err != nil {
		return fmt.Errorf("failed to add virtiofs: %w", err)
	}

	return nil
}

func (v *vmInstance) AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode vm.NetworkMode, features, flags uint32) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if err := v.vmc.AddNIC(endpoint, mac, mode, features, flags); err != nil {
		return fmt.Errorf("failed to add nic: %w", err)
	}

	return nil
}

func (v *vmInstance) Start(ctx context.Context, opts ...vm.StartOpt) (err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.client != nil {
		return errors.New("VM instance already started")
	}

	if err := v.vmc.SetCPUAndMemory(2, 2096); err != nil {
		return fmt.Errorf("failed to set cpu and memory: %w", err)
	}
	if err := v.vmc.SetKernel(v.kernelPath, v.initrdPath, "console=hvc0"); err != nil {
		return fmt.Errorf("failed to set kernel: %w", err)
	}

	env := []string{
		"TERM=xterm",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
	}

	startOpts := vm.StartOpts{
		InitArgs: []string{
			"-debug",
			"-vsock-rpc-port=1025",    // vsock rpc port number
			"-vsock-stream-port=1026", // vsock stream port number
			"-vsock-cid=3",            // vsock guest context id
		},
	}
	for _, o := range opts {
		o(&startOpts)
	}

	if err := v.vmc.SetExec("/sbin/vminitd", startOpts.InitArgs, env); err != nil {
		return fmt.Errorf("failed to set exec: %w", err)
	}

	cf := "./krun.fifo"
	lr, err := fifo.OpenFifo(ctx, cf, os.O_RDONLY|os.O_CREATE|syscall.O_NONBLOCK, 0644)
	if err != nil {
		return err
	}
	if err := v.vmc.SetConsole(cf); err != nil {
		return fmt.Errorf("failed to set console: %w", err)
	}
	go io.Copy(os.Stderr, lr)

	// Consider not using unix sockets here and directly connecting via vsock
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get cwd: %w", err)
	}
	socketPath := filepath.Join(v.state, "run_vminitd.sock")
	socketPath, err = filepath.Rel(cwd, socketPath)
	if err != nil {
		return fmt.Errorf("failed to get relative socket path: %w", err)
	}
	if err := v.vmc.AddVSockPort(1025, socketPath); err != nil {
		return fmt.Errorf("failed to add vsock port: %w", err)
	}

	v.streamPath, err = filepath.Rel(cwd, v.streamPath)
	if err != nil {
		return fmt.Errorf("failed to get relative socket path: %w", err)
	}
	if err := v.vmc.AddVSockPort(1026, v.streamPath); err != nil {
		return fmt.Errorf("failed to add vsock port: %w", err)
	}

	// Start it
	errC := make(chan error)
	go func() {
		defer close(errC)
		if err := v.vmc.Start(); err != nil {
			errC <- err
		}
	}()

	v.shutdownCallbacks = []func(context.Context) error{
		func(context.Context) error {
			cerr := v.vmc.Shutdown()
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
	startedAt := time.Now()
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
		if time.Since(startedAt) > vmStartTimeout {
			log.G(ctx).WithField("timeout", vmStartTimeout).Warn("Timeout while waiting for VM to start")
			return fmt.Errorf("VM did not start within %s", vmStartTimeout)
		}
		if _, err := os.Stat(socketPath); err == nil {
			conn, err = net.Dial("unix", socketPath)
			if err != nil {
				return fmt.Errorf("failed to connect to TTRPC server: %w", err)
			}
			conn.SetReadDeadline(time.Now().Add(d))
			if err := ttrpcutil.PingTTRPC(conn); err != nil {
				conn.Close()
				d = d + time.Millisecond
				continue
			}

			conn.SetReadDeadline(time.Time{}) // Clear the deadline
			// Ensure connection alive after deadline is cleared
			if err := ttrpcutil.PingTTRPC(conn); err != nil {
				conn.Close()
				continue
			}

			v.shutdownCallbacks = append(v.shutdownCallbacks, func(context.Context) error {
				return conn.Close()
			})
			break
		}
	}

	v.client = ttrpc.NewClient(conn)

	return nil
}

func (v *vmInstance) StartStream(ctx context.Context) (uint32, net.Conn, error) {
	var conn net.Conn
	const timeIncrement = 10 * time.Millisecond
	for d := timeIncrement; d < time.Second; d += timeIncrement {
		sid := atomic.AddUint32(&v.streamC, 1)
		if sid == 0 {
			return 0, nil, fmt.Errorf("exhausted stream identifiers: %w", errdefs.ErrUnavailable)
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		default:
		}
		if _, err := os.Stat(v.streamPath); err == nil {
			conn, err = net.Dial("unix", v.streamPath)
			if err != nil {
				return 0, nil, fmt.Errorf("failed to connect to stream server: %w", err)
			}
			var vs [4]byte
			binary.BigEndian.PutUint32(vs[:], sid)
			if _, err := conn.Write(vs[:]); err != nil {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to write stream id to stream server: %w", err)
			}
			// Wait for ack
			var ack [4]byte
			if _, err := io.ReadFull(conn, ack[:]); err != nil {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to read ack from stream server: %w", err)
			}
			if binary.BigEndian.Uint32(ack[:]) != sid {
				conn.Close()
				return 0, nil, fmt.Errorf("stream server ack mismatch: got %d, expected %d", binary.BigEndian.Uint32(ack[:]), sid)
			}

			return sid, conn, nil
		}
	}
	return 0, nil, fmt.Errorf("timeout waiting for stream server: %w", errdefs.ErrUnavailable)
}

func (v *vmInstance) Client() *ttrpc.Client {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.client
}

func (v *vmInstance) Shutdown(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.handler == 0 {
		return fmt.Errorf("libkrun already closed")
	}
	err := purego.Dlclose(v.handler)
	if err != nil {
		return err
	}
	v.handler = 0 // Mark as closed
	return nil
}

func kernelArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}
