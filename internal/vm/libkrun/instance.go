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
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	"github.com/containerd/nerdbox/internal/vm"
)

var vmStartTimeout = 15 * time.Second

func init() {
	if runtime.GOOS == "windows" {
		// Windows WHP hypervisor has higher startup overhead than macOS/Linux.
		vmStartTimeout = 30 * time.Second
	}
}

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
	if runtime.GOOS != "windows" && len(p2) == 0 {
		p2 = []string{"/usr/local/lib", "/usr/local/lib64", "/usr/lib", "/lib"}
	}
	sharedNames := []string{"libkrun.so"}
	switch runtime.GOOS {
	case "darwin":
		sharedNames = []string{"libkrun.dylib", "libkrun-efi.dylib"}
		p2 = append(p2, "/opt/homebrew/lib")
	case "windows":
		sharedNames = []string{"krun.dll"}
	}

	for _, dir := range append(p1, p2...) {
		if dir == "" {
			// Unix shell semantics: path element "" means "."
			dir = "."
		}
		var path string
		if krunPath == "" {
			for _, sharedName := range sharedNames {
				path = filepath.Join(dir, sharedName)
				if _, err := os.Stat(path); err == nil {
					krunPath = path
					break
				}
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
		return nil, fmt.Errorf("%s not found in PATH or LIBKRUN_PATH", strings.Join(sharedNames, " or "))
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
		ret = lib.InitLog(os.Stderr.Fd(), uint32(warnLevel), 0, 0)
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
		return fmt.Errorf("failed to add virtiofs tag:%s mount:%s: %w", tag, mountPath, err)
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

	var dskFmt uint32 = 0
	if mc.Vmdk {
		dskFmt = 2
	}
	if err := v.vmc.AddDisk2(blockID, mountPath, dskFmt, mc.Readonly); err != nil {
		return fmt.Errorf("failed to add disk at '%s': %w", mountPath, err)
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

func (v *vmInstance) SetCPUAndMemory(ctx context.Context, cpu uint8, ram uint32) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if err := v.vmc.SetCPUAndMemory(cpu, ram); err != nil {
		return fmt.Errorf("failed to set cpu and memory: %w", err)
	}

	return nil
}

func (v *vmInstance) Start(ctx context.Context, opts ...vm.StartOpt) (err error) {
	startedAt := time.Now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.client != nil {
		return errors.New("VM instance already started")
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
	lr, err := setupConsole(ctx, v.vmc, cf)
	if err != nil {
		return fmt.Errorf("failed to set up console: %w", err)
	}
	if lr != nil {
		consoleW := io.Writer(os.Stderr)
		if startOpts.ConsoleWriter != nil {
			consoleW = io.MultiWriter(os.Stderr, startOpts.ConsoleWriter)
		}
		go io.Copy(consoleW, lr)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get cwd: %w", err)
	}
	socketPath := filepath.Join(v.state, "run_vminitd.sock")
	// Compute the relative socket path to avoid exceeding the max length on macOS.
	socketPath, err = filepath.Rel(cwd, socketPath)
	if err != nil {
		return fmt.Errorf("failed to get relative socket path: %w", err)
	}
	if (runtime.GOOS == "darwin" && len(socketPath) >= 104) || len(socketPath) >= 108 {
		return fmt.Errorf("socket path is too long: %s", socketPath)
	}

	// Listen on the unix socket so vminitd can connect back to us.
	// AddVSockPortConnect (listen=false) tells libkrun to connect to this
	// socket when the guest dials the vsock port, bridging the connection.
	// Remove any stale socket left behind by a previous crash.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale socket: %w", err)
	}
	rpcListener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}
	defer rpcListener.Close()

	if err := v.vmc.AddVSockPortConnect(1025, socketPath); err != nil {
		return fmt.Errorf("failed to add vsock port: %w", err)
	}

	v.streamPath, err = filepath.Rel(cwd, v.streamPath)
	if err != nil {
		return fmt.Errorf("failed to get relative socket path: %w", err)
	}
	if err := v.vmc.AddVSockPort(1026, v.streamPath); err != nil {
		return fmt.Errorf("failed to add vsock port: %w", err)
	}

	preVMStart := time.Now()

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

	// Accept a single connection from vminitd connecting back via vsock.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	acceptC := make(chan acceptResult, 1)
	go func() {
		conn, err := rpcListener.Accept()
		acceptC <- acceptResult{conn, err}
	}()

	var conn net.Conn
	select {
	case err := <-errC:
		if err != nil {
			return fmt.Errorf("failure running vm: %w", err)
		}
		return fmt.Errorf("VM exited before connecting")
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(vmStartTimeout):
		log.G(ctx).WithField("timeout", vmStartTimeout).Warn("Timeout while waiting for VM to connect")
		return fmt.Errorf("VM did not connect within %s", vmStartTimeout)
	case result := <-acceptC:
		if result.err != nil {
			return fmt.Errorf("failed to accept connection from VM: %w", result.err)
		}
		conn = result.conn
	}

	log.G(ctx).WithFields(log.Fields{
		"t_config": preVMStart.Sub(startedAt),
		"t_boot":   time.Since(preVMStart),
		"t_total":  time.Since(startedAt),
	}).Info("VM connection established")

	v.shutdownCallbacks = append(v.shutdownCallbacks, func(context.Context) error {
		return conn.Close()
	})

	v.client = ttrpc.NewClient(conn)

	return nil
}

func (v *vmInstance) StartStream(ctx context.Context, streamID string) (net.Conn, error) {
	const timeIncrement = 10 * time.Millisecond
	for d := timeIncrement; d < time.Second; d += timeIncrement {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if _, err := os.Stat(v.streamPath); err == nil {
			conn, err := net.Dial("unix", v.streamPath)
			if err != nil {
				return nil, fmt.Errorf("failed to connect to stream server: %w", err)
			}
			// Write length-prefixed stream ID
			idBytes := []byte(streamID)
			if err := binary.Write(conn, binary.BigEndian, uint32(len(idBytes))); err != nil {
				conn.Close()
				return nil, fmt.Errorf("failed to write stream id length: %w", err)
			}
			if _, err := conn.Write(idBytes); err != nil {
				conn.Close()
				return nil, fmt.Errorf("failed to write stream id: %w", err)
			}
			// Wait for ack (length-prefixed string echoed back)
			var ackLen uint32
			if err := binary.Read(conn, binary.BigEndian, &ackLen); err != nil {
				conn.Close()
				return nil, fmt.Errorf("failed to read ack length: %w", err)
			}
			ackBytes := make([]byte, ackLen)
			if _, err := io.ReadFull(conn, ackBytes); err != nil {
				conn.Close()
				return nil, fmt.Errorf("failed to read ack: %w", err)
			}
			if ack := string(ackBytes); ack != streamID {
				conn.Close()
				return nil, fmt.Errorf("stream %q rejected by server: %s", streamID, ack)
			}

			return conn, nil
		}
		time.Sleep(d)
	}
	return nil, fmt.Errorf("timeout waiting for stream server: %w", errdefs.ErrUnavailable)
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
	err := dlClose(v.handler)
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
