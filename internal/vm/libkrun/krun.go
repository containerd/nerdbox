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
	"fmt"
	"net"
	"reflect"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/containerd/nerdbox/internal/vm"
)

const (
	VirglrendererVenus   = 1 << 6
	VirglrendererNoVirgl = 1 << 7
)

const (
	kernelFormatRaw = 0
	kernelFormatElf = 1

// #define KRUN_KERNEL_FORMAT_PE_GZ 2
// #define KRUN_KERNEL_FORMAT_IMAGE_BZ2 3
// #define KRUN_KERNEL_FORMAT_IMAGE_GZ 4
// #define KRUN_KERNEL_FORMAT_IMAGE_ZSTD 5
)

type logLevel uint32

const (
	debugLevel logLevel = 1
)

type vmcontext struct {
	ctxID uint32
	lib   *libkrun

	// Track passed down strings
	passedDown [][]byte
}

func newvmcontext(lib *libkrun) (*vmcontext, error) {
	// Start VM context
	ctxId := lib.CreateCtx()
	if ctxId < 0 {
		return nil, fmt.Errorf("krun_create_ctx failed: %d", ctxId)
	}

	return &vmcontext{
		ctxID: uint32(ctxId),
		lib:   lib,
	}, nil
}

func (vmc *vmcontext) SetCPUAndMemory(cpu uint8, ram uint32) error {
	if vmc.lib.SetVMConfig == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vmc.lib.SetVMConfig(vmc.ctxID, cpu, ram)
	if ret != 0 {
		return fmt.Errorf("krun_set_vm_config failed: %d", ret)
	}
	return nil
}

func (vmc *vmcontext) SetKernel(kernelPath string, initrdPath string, kernelCmdline string) error {
	if vmc.lib.SetKernel == nil {
		return fmt.Errorf("libkrun not loaded")
	}

	// TODO: Support different kernel formats
	var format uint32
	if runtime.GOARCH == "arm64" {
		format = kernelFormatRaw
	} else {
		format = kernelFormatElf
	}
	ret := vmc.lib.SetKernel(vmc.ctxID, kernelPath, format, initrdPath, kernelCmdline)
	if ret != 0 {
		return fmt.Errorf("krun_set_kernel failed: %d", ret)
	}
	return nil
}

func (vmc *vmcontext) SetExec(path string, args []string, env []string) error {
	if vmc.lib.SetExec == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vmc.lib.SetExec(vmc.ctxID, path, vmc.cStringArray(args), vmc.cStringArray(env))
	if ret != 0 {
		return fmt.Errorf("krun_set_exec failed: %d", ret)
	}
	return nil
}

func (vmc *vmcontext) SetConsole(path string) error {
	if vmc.lib.SetConsoleOutput == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vmc.lib.SetConsoleOutput(vmc.ctxID, path)
	if ret != 0 {
		return fmt.Errorf("krun_set_console_output failed: %d", ret)
	}
	return nil
}

func (vmc *vmcontext) AddVSockPort(port uint32, path string) error {
	if vmc.lib.AddVsockPort == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vmc.lib.AddVsockPort(vmc.ctxID, port, path, true)
	if ret != 0 {
		return fmt.Errorf("krun_add_vsock_port failed: %d", ret)
	}
	return nil
}

func (vmc *vmcontext) AddVirtiofs(tag, path string) error {
	if vmc.lib.AddVirtiofs == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vmc.lib.AddVirtiofs(vmc.ctxID, tag, path)
	if ret != 0 {
		return fmt.Errorf("krun_add_virtio_fs failed: %d", ret)
	}
	return nil
}

func (vmc *vmcontext) AddDisk(blockID, path string, readonly bool) error {
	if vmc.lib.AddDisk == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vmc.lib.AddDisk(vmc.ctxID, blockID, path, readonly)
	if ret != 0 {
		return fmt.Errorf("krun_add_disk failed: %d", ret)
	}
	return nil
}

func (vmc *vmcontext) AddNIC(endpoint string, mac net.HardwareAddr, mode vm.NetworkMode, features, flags uint32) error {
	if vmc.lib.AddNetUnixgram == nil || vmc.lib.AddNetUnixstream == nil {
		return fmt.Errorf("libkrun not loaded")
	}

	switch mode {
	case vm.NetworkModeUnixgram:
		ret := vmc.lib.AddNetUnixgram(vmc.ctxID, endpoint, -1, []uint8(mac), features, flags)
		if ret != 0 {
			return fmt.Errorf("krun_add_net_unixgram failed: %d", ret)
		}
	case vm.NetworkModeUnixstream:
		ret := vmc.lib.AddNetUnixstream(vmc.ctxID, endpoint, -1, []uint8(mac), features, flags)
		if ret != 0 {
			return fmt.Errorf("krun_add_net_unixstream failed: %d", ret)
		}
	default:
		return fmt.Errorf("invalid network mode: %d", mode)
	}

	return nil
}

func (vmc *vmcontext) Start() error {
	if vmc.lib.StartEnter == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vmc.lib.StartEnter(vmc.ctxID)
	if ret != 0 {
		return fmt.Errorf("krun_start_enter failed: %d", ret)
	}
	return nil
}

func (vmc *vmcontext) Shutdown() error {
	if vmc.ctxID == 0 {
		return nil
	}
	ret := vmc.lib.FreeCtx(vmc.ctxID)
	if ret != 0 {
		return fmt.Errorf("krun_free_ctx failed: %d", ret)
	}
	vmc.ctxID = 0
	return nil
}

func (vmc *vmcontext) cString(a string) unsafe.Pointer {
	if a == "" {
		return nil
	}
	if !strings.HasSuffix(a, "\000") {
		a += "\000"
	}
	b := []byte(a)
	vmc.passedDown = append(vmc.passedDown, b)
	return unsafe.Pointer(unsafe.SliceData(b))
}

func (vmc *vmcontext) cStringArray(a []string) unsafe.Pointer {
	if len(a) == 0 {
		return nil
	}
	o := make([]unsafe.Pointer, len(a)+1)
	o[len(a)] = nil // Null-terminate the array
	for i := range a {
		o[i] = vmc.cString(a[i])
	}
	return unsafe.Pointer(unsafe.SliceData(o))
}

type libkrun struct {
	SetLogLevel        func(level uint32) int32                                                               `C:"krun_set_log_level"`
	InitLog            func(fd uintptr, level uint32, style uint32, options uint32) int32                     `C:"krun_init_log"`
	CreateCtx          func() int32                                                                           `C:"krun_create_ctx"`
	FreeCtx            func(ctxID uint32) int32                                                               `C:"krun_free_ctx"`
	SetVMConfig        func(ctxID uint32, cpu uint8, ram uint32) int32                                        `C:"krun_set_vm_config"`
	SetKernel          func(ctxID uint32, path string, format uint32, initramfs string, cmdline string) int32 `C:"krun_set_kernel"`
	SetExec            func(ctxID uint32, path string, args unsafe.Pointer, env unsafe.Pointer) int32         `C:"krun_set_exec"`
	SetConsoleOutput   func(ctxID uint32, path string) int32                                                  `C:"krun_set_console_output"`
	StartEnter         func(ctxID uint32) int32                                                               `C:"krun_start_enter"`
	AddVsockPort       func(ctxID, port uint32, path string, listen bool) int32                               `C:"krun_add_vsock_port2"`
	AddVirtiofs        func(ctxID uint32, tag, path string) int32                                             `C:"krun_add_virtiofs"`
	GetShutdownEventfd func(ctxID uint32) int32                                                               `C:"krun_get_shutdown_eventfd"`
	SetGpuOptions      func(ctxID, flag uint32) int32                                                         `C:"krun_set_gpu_options"`
	SetGvproxyPath     func(ctxID uint32, path string) int32                                                  `C:"krun_set_gvproxy_path"`
	SetNetMac          func(ctxID uint32, mac []uint8) int32                                                  `C:"krun_set_net_mac"`
	AddDisk            func(ctxID uint32, blockId, path string, readonly bool) int32                          `C:"krun_add_disk"`
	AddNetUnixstream   func(ctxID uint32, path string, fd int, mac []uint8, features, flags uint32) int32     `C:"krun_add_net_unixstream"`
	AddNetUnixgram     func(ctxID uint32, path string, fd int, mac []uint8, features, flags uint32) int32     `C:"krun_add_net_unixgram"`

	/*
		All functions (As of July 2025)
		krun_setuid
		krun_set_log_level
		krun_create_ctx
		krun_init_log
		krun_set_gpu_options
		krun_set_root
		krun_check_nested_virt
		krun_set_mapped_volumes
		krun_set_exec
		krun_add_vsock_port
		krun_set_vm_config
		krun_add_virtiofs2
		krun_set_console_output
		krun_set_env
		krun_set_gvproxy_path
		krun_add_disk2
		krun_setgid
		krun_set_rlimits
		krun_set_snd_device
		krun_split_irqchip
		krun_set_data_disk
		krun_set_kernel
		bz_internal_error
		krun_start_enter
		krun_set_port_map
		krun_set_workdir
		krun_add_virtiofs
		krun_free_ctx
		krun_set_net_mac
		krun_add_disk
		krun_get_shutdown_eventfd
		krun_set_smbios_oem_strings
		krun_set_passt_fd
		krun_set_gpu_options2
		krun_set_nested_virt
		krun_add_vsock_port2
		krun_set_root_disk
	*/
}

func openLibkrun(path string) (_ *libkrun, _ uintptr, retErr error) {
	f, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, 0, err
	}

	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(error); ok {
				retErr = e
			} else {
				retErr = fmt.Errorf("panic while loading libkrun: %v", p)
			}
		}
		if retErr != nil {
			purego.Dlclose(f)
		}
	}()
	var k libkrun
	ik := reflect.Indirect(reflect.ValueOf(&k))
	for i := 0; i < ik.NumField(); i++ {
		cName := ik.Type().Field(i).Tag.Get("C")
		fn := ik.Field(i).Addr().Interface()
		purego.RegisterLibFunc(fn, f, cName)
	}

	return &k, f, nil
}
