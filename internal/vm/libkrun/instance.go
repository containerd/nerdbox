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
		return nil, fmt.Errorf("krun_init_log failed: %d", ret)
	}

	vmc, err := newvmcontext(lib)
	if err != nil {
		return nil, err
	}

	// Add the erofs rootfs as the first virtio-blk device so that it is
	// always exposed as /dev/vda inside the guest.  Container image disks
	// are added later via AddDisk, which appends to the device list, so
	// they receive /dev/vdb, /dev/vdc, … in order of addition.
	if err := vmc.AddDisk2("vmrootfs", rootfsPath, 0, true); err != nil {
		return nil, fmt.Errorf("failed to add VM rootfs disk %q: %w", rootfsPath, err)
	}

	return &vmInstance{
		vmc:                vmc,
		state:              state,
		kernelPath:         kernelPath,
		rootfsPath:         rootfsPath,
		streamPath:         filepath.Join(state, "streaming.sock"),
		lib:                lib,
		handler:            handler,
		inFlightHandshakes: make(map[net.Conn]struct{}),
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

	client *ttrpc.Client
	conn   net.Conn // underlying TTRPC connection; closed in Shutdown

	// inFlightHandshakes lets Shutdown close every dialed-but-unclaimed
	// StartStream connection, so a handshake blocked on a guest that never
	// acks returns instead of hanging for the VM's lifetime. Shutdown also
	// nils this out, so a StartStream that dials afterward fails fast
	// rather than registering a connection nothing will ever close.
	inFlightHandshakes map[net.Conn]struct{}

	// shuttingDown is set while Shutdown is tearing the instance down.
	shuttingDown bool
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

	// Start it
	errC := make(chan error, 1)
	go func() {
		defer close(errC)
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

			if !v.trackStreamConn(conn) {
				// Shutdown has already closed every tracked stream
				// connection and is tearing down (or has torn down) the
				// VM; don't hand back a connection racing that teardown.
				conn.Close()
				return nil, errdefs.ErrUnavailable.WithMessage("vm instance is shutting down")
			}

			handshakeErr := completeStreamHandshake(conn, streamID)
			if !v.untrackStreamConn(conn) {
				// Shutdown's closeStreamConnsLocked ran while the handshake
				// was in flight (or in the instant after it finished) and
				// may have closed this exact conn out from under us, even
				// though handshakeErr came back nil; don't hand back a
				// connection that could already be dead.
				conn.Close()
				return nil, errdefs.ErrUnavailable.WithMessage("vm instance is shutting down")
			}
			if handshakeErr != nil {
				conn.Close()
				return nil, handshakeErr
			}

			return conn, nil
		}
		time.Sleep(d)
	}
	return nil, errdefs.ErrUnavailable.WithMessage("timed out waiting for stream server")
}

// completeStreamHandshake has no deadline of its own: a caller that wants
// it to return when the VM goes away must close conn out from under it
// (see closeStreamConnsLocked), which unblocks the pending Write or Read
// with an error.
func completeStreamHandshake(conn net.Conn, streamID string) error {
	idBytes := []byte(streamID)
	if err := binary.Write(conn, binary.BigEndian, uint32(len(idBytes))); err != nil {
		return fmt.Errorf("failed to write stream id length: %w", err)
	}
	if _, err := conn.Write(idBytes); err != nil {
		return fmt.Errorf("failed to write stream id: %w", err)
	}
	var ackLen uint32
	if err := binary.Read(conn, binary.BigEndian, &ackLen); err != nil {
		return fmt.Errorf("failed to read ack length: %w", err)
	}
	ackBytes := make([]byte, ackLen)
	if _, err := io.ReadFull(conn, ackBytes); err != nil {
		return fmt.Errorf("failed to read ack: %w", err)
	}
	if ack := string(ackBytes); ack != streamID {
		return fmt.Errorf("stream %q rejected by server: %s", streamID, ack)
	}
	return nil
}

// trackStreamConn registers conn so closeStreamConnsLocked can close it if
// it is still in flight when the VM is torn down. It returns false once
// Shutdown has run (or is running), in which case conn was never
// registered and the caller must close it itself.
func (v *vmInstance) trackStreamConn(conn net.Conn) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.inFlightHandshakes == nil {
		return false
	}
	v.inFlightHandshakes[conn] = struct{}{}
	return true
}

// untrackStreamConn removes conn from the set closeStreamConnsLocked would
// close, and reports whether conn was still tracked (i.e. Shutdown had not
// yet run closeStreamConnsLocked as of this call). A false return means
// Shutdown already ran and cleared the set, so conn may already have been
// closed even if it was tracked when this call started — the caller must
// not treat conn as usable in that case.
func (v *vmInstance) untrackStreamConn(conn net.Conn) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.inFlightHandshakes == nil {
		return false
	}
	delete(v.inFlightHandshakes, conn)
	return true
}

// closeStreamConnsLocked lets a StartStream call blocked in
// completeStreamHandshake return with an error instead of hanging once
// the VM it depends on is gone. Callers must hold v.mu.
func (v *vmInstance) closeStreamConnsLocked() {
	for conn := range v.inFlightHandshakes {
		conn.Close()
	}
	v.inFlightHandshakes = nil
}

func (v *vmInstance) Client() *ttrpc.Client {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.client
}

func (v *vmInstance) Shutdown(ctx context.Context) error {
	err := v.beginShutdown()
	if err != nil {
		return err
	}

	// shuttingDown clears once the teardown below finishes, successfully
	// or not, so a caller that gets an error back (e.g. from dlClose) can
	// retry instead of finding this instance permanently stuck rejecting
	// every Shutdown call.
	defer func() {
		v.mu.Lock()
		v.shuttingDown = false
		v.mu.Unlock()
	}()

	v.mu.Lock()

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

	// Run the shutdown of the vm context outside of the critical section to
	// allow concurrent operations to run/stop gracefully.
	v.mu.Unlock()

	// Stop the VM. krun_free_ctx joins all VM threads (vCPU, virtio workers)
	// on most platforms. On Windows WHP it initiates the stop but may return
	// before krun_start_enter unblocks; the goroutine is cleaned up on exit.
	if v.vmc != nil {
		if err := v.vmc.Shutdown(); err != nil {
			log.G(ctx).WithError(err).Warn("krun_free_ctx failed during shutdown")
		}
	}

	// On Unix, dlClose unloads the library after krun_free_ctx has joined all
	// VM threads. On Windows it is a no-op (see dlfcn_windows.go).
	v.mu.Lock()
	handler := v.handler
	v.mu.Unlock()

	if err := dlClose(handler); err != nil {
		return err
	}

	v.mu.Lock()
	v.handler = 0
	v.mu.Unlock()
	return nil
}

// beginShutdown validates the instance can be shutdown and marks it
// as shutdown in progress to prevent concurrent shutdowns from occurring.
func (v *vmInstance) beginShutdown() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.handler == 0 {
		return errors.New("libkrun already closed")
	}
	if v.shuttingDown {
		return errors.New("libkrun already shutting down")
	}
	v.shuttingDown = true
	v.closeStreamConnsLocked()
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
