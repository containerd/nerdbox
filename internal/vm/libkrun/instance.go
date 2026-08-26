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

	"github.com/containerd/nerdbox/internal/kvm"
	"github.com/containerd/nerdbox/pkg/logging"
	"github.com/containerd/nerdbox/pkg/vm"
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

// ReservedDisks returns 1 because the libkrun shim always attaches the
// erofs rootfs image as the first virtio-blk device (/dev/vda) in
// NewInstance, before any container-supplied disks are added.
func (*vmManager) ReservedDisks() int { return 1 }

func (*vmManager) NewInstance(ctx context.Context, state string) (vm.Instance, error) {
	// On Linux, libkrun panics if KVM is not available, so check it here.
	if err := kvm.CheckKVM(); err != nil {
		return nil, err
	}

	var (
		p1         = filepath.SplitList(os.Getenv("PATH"))
		p2         = filepath.SplitList(os.Getenv("LIBKRUN_PATH"))
		krunPath   string
		kernelPath string
		rootfsPath string
	)
	if runtime.GOOS != "windows" && len(p2) == 0 {
		p2 = []string{"/usr/local/lib", "/usr/local/lib64", "/usr/lib", "/lib"}
	}
	arch := kernelArch()
	sharedNames := []string{fmt.Sprintf("libkrun-%s.so", arch), "libkrun.so"}
	switch runtime.GOOS {
	case "darwin":
		sharedNames = []string{fmt.Sprintf("libkrun-%s.dylib", arch), "libkrun.dylib", fmt.Sprintf("libkrun-efi-%s.dylib", arch), "libkrun-efi.dylib"}
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
		if rootfsPath == "" {
			for _, name := range []string{fmt.Sprintf("nerdbox-rootfs-%s.erofs", arch), "nerdbox-rootfs.erofs"} {
				path = filepath.Join(dir, name)
				if _, err := os.Stat(path); err == nil {
					rootfsPath = path
					break
				}
			}
		}
	}
	if krunPath == "" {
		return nil, fmt.Errorf("%s not found in PATH or LIBKRUN_PATH", strings.Join(sharedNames, " or "))
	}
	if kernelPath == "" {
		return nil, fmt.Errorf("nerdbox-kernel not found in PATH or LIBKRUN_PATH")
	}
	if rootfsPath == "" {
		return nil, fmt.Errorf("nerdbox-rootfs-%s.erofs or nerdbox-rootfs.erofs not found in PATH or LIBKRUN_PATH", arch)
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
		_ = dlClose(handler)
		return nil, fmt.Errorf("krun_init_log failed: %d", ret)
	}

	vmc, err := newvmcontext(lib)
	if err != nil {
		_ = dlClose(handler)
		return nil, err
	}

	// Add the erofs rootfs as the first virtio-blk device so that it is
	// always exposed as /dev/vda inside the guest. Container-supplied
	// disks are added later via AddDisk, which appends to the device
	// list, so they receive /dev/vdb, /dev/vdc, … in order of addition.
	if err := vmc.AddDisk2("vmrootfs", rootfsPath, 0, true); err != nil {
		_ = dlClose(handler)
		return nil, fmt.Errorf("failed to add VM rootfs disk %q: %w", rootfsPath, err)
	}

	return &vmInstance{
		vmc:        vmc,
		state:      state,
		kernelPath: kernelPath,
		rootfsPath: rootfsPath,
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
	rootfsPath string
	streamPath string

	lib     *libkrun
	handler uintptr

	// netnsSet/netns record the pod network namespace requested by the
	// first call to Start (successful or not), so that a subsequent Start
	// attempt (e.g. a retry after a failed one) can be validated against
	// it: a repeated request for the same (or no) namespace is a no-op,
	// but a request for a different namespace is rejected outright rather
	// than silently ignored, since that would hide a real caller bug.
	netnsSet bool
	netns    string

	client *ttrpc.Client
	conn   net.Conn // underlying TTRPC connection; closed in Shutdown
}

// resolveNetNS validates a Start-requested network namespace against the
// namespace recorded by an earlier Start attempt on this instance, if any
// (for example, a retry after a Start call that failed before reaching the
// network-namespace switch). The same namespace, or none at all, is a
// no-op; a genuinely different, non-empty namespace after one was already
// recorded is rejected rather than silently overriding the first request,
// since that would hide a caller bug. The caller must hold v.mu.
func (v *vmInstance) resolveNetNS(requested string) error {
	if v.netnsSet {
		if requested != "" && requested != v.netns {
			return fmt.Errorf("cannot change VM netns after it was already set to %q: got %q", v.netns, requested)
		}
		return nil
	}
	v.netns = requested
	v.netnsSet = true
	return nil
}

func (v *vmInstance) AddFS(ctx context.Context, tag, mountPath string, opts ...vm.MountOpt) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// TODO: Cannot be started?

	var mc vm.MountConfig
	for _, o := range opts {
		o(&mc)
	}

	if err := v.vmc.AddVirtiofs(tag, mountPath, mc.Readonly); err != nil {
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

func (v *vmInstance) AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode vm.NetworkMode, opts ...vm.NetworkOpt) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	var no vm.NetworkOpts
	for _, o := range opts {
		o(&no)
	}

	if err := v.vmc.AddNIC(endpoint, mac, mode, no.Features, no.Flags); err != nil {
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

	// Boot directly from the erofs rootfs block device (/dev/vda).
	// No initrd is needed: the kernel mounts the erofs image as the
	// root filesystem and launches vminitd directly as PID 1.
	const kernelCmdline = "console=hvc0 root=/dev/vda rootfstype=erofs ro init=/sbin/vminitd"
	if err := v.vmc.SetKernel(v.kernelPath, "", kernelCmdline); err != nil {
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

	if err := v.resolveNetNS(startOpts.NetNS); err != nil {
		return err
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
		go logging.ForwardConsoleLogs(lr, startOpts.ConsoleWriter)
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

	// Start it.
	//
	// runtime.LockOSThread pins this goroutine to one OS thread for the
	// VM's entire lifetime (krun_start_enter blocks until the VM shuts
	// down). This is necessary for two reasons:
	//   1. setns(2) affects only the calling OS thread; without
	//      LockOSThread the goroutine could migrate to a different
	//      thread and the setns would be lost before krun_start_enter is
	//      reached.
	//   2. libkrun's worker threads (vCPU, virtio backends, vsock/TSI
	//      workers), which krun_start_enter spawns as descendants of the
	//      calling thread, inherit the netns of that thread. They must be
	//      created in the pod netns so that VM traffic (including TSI
	//      proxy sockets) lands there.
	//
	// We deliberately do NOT call runtime.UnlockOSThread. When a
	// goroutine that holds a thread lock exits, the Go runtime retires
	// the underlying OS thread (Go 1.10+), so there is no thread-pool
	// "poisoning" concern, and the pod-netns thread is never returned to
	// the pool where it could pollute the default netns.
	errC := make(chan error, 1)
	go func() {
		defer close(errC)
		runtime.LockOSThread()
		if v.netns != "" {
			if err := vmcontextSetNetns(v.netns); err != nil {
				errC <- fmt.Errorf("entering pod netns: %w", err)
				return
			}
			// Log the resulting thread netns inode so it can be
			// cross-checked against the pod netns inode when debugging
			// connectivity issues.
			inode, _ := os.Readlink("/proc/thread-self/ns/net")
			log.G(ctx).WithFields(log.Fields{
				"netns_path":  v.netns,
				"netns_inode": inode,
			}).Debug("VM start thread entered pod netns")
		}
		if err := v.vmc.Start(); err != nil {
			errC <- err
		}
	}()

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

	v.conn = conn
	v.client = ttrpc.NewClient(conn)

	return nil
}

func (v *vmInstance) StartStream(ctx context.Context, streamID string, _ ...vm.StreamOpt) (net.Conn, error) {
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

	// Close the TTRPC client so in-flight RPCs fail fast and its background
	// goroutines are stopped before we tear down the connection underneath.
	if v.client != nil {
		v.client.Close()
		v.client = nil
	}

	// Close the underlying TTRPC net.Conn to vminitd. This must happen
	// before krun_free_ctx to avoid leaving file handles open, which would
	// prevent containerd from cleaning up the bundle directory.
	if v.conn != nil {
		if err := v.conn.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close TTRPC connection")
		}
		v.conn = nil
	}

	// Stop the VM. krun_free_ctx joins all VM threads (vCPU, virtio workers)
	// on most platforms. On Windows WHP it initiates the stop but may return
	// before krun_start_enter unblocks; the goroutine is cleaned up on exit.
	if v.vmc != nil {
		if err := v.vmc.Shutdown(); err != nil {
			log.G(ctx).WithError(err).Warn("krun_free_ctx failed during shutdown")
		}
	}

	// On Unix, dlClose unloads the library after krun_free_ctx has joined
	// all VM threads. On Windows it is a no-op (see dlfcn_windows.go).
	if err := dlClose(v.handler); err != nil {
		return err
	}
	v.handler = 0
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
