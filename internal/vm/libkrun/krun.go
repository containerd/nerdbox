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
	"reflect"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
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
	ctxId uint32
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
		ctxId: uint32(ctxId),
		lib:   lib,
	}, nil
}

func (vm *vmcontext) SetCPUAndMemory(cpu uint8, ram uint32) error {
	if vm.lib.SetVmConfig == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vm.lib.SetVmConfig(vm.ctxId, cpu, ram)
	if ret != 0 {
		return fmt.Errorf("krun_set_vm_config failed: %d", ret)
	}
	return nil
}

func (vm *vmcontext) SetKernel(kernelPath string, initrdPath string, kernelCmdline string) error {
	if vm.lib.SetKernel == nil {
		return fmt.Errorf("libkrun not loaded")
	}

	// TODO: Support different kernel formats
	var format uint32
	if runtime.GOARCH == "arm64" {
		format = kernelFormatRaw
	} else {
		format = kernelFormatElf
	}
	ret := vm.lib.SetKernel(vm.ctxId, kernelPath, format, initrdPath, kernelCmdline)
	if ret != 0 {
		return fmt.Errorf("krun_set_kernel failed: %d", ret)
	}
	return nil
}

func (vm *vmcontext) SetExec(path string, args []string, env []string) error {
	if vm.lib.SetExec == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vm.lib.SetExec(vm.ctxId, path, vm.cStringArray(args), vm.cStringArray(env))
	if ret != 0 {
		return fmt.Errorf("krun_set_exec failed: %d", ret)
	}
	return nil
}

func (vm *vmcontext) SetConsole(path string) error {
	if vm.lib.SetConsoleOutput == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vm.lib.SetConsoleOutput(vm.ctxId, path)
	if ret != 0 {
		return fmt.Errorf("krun_set_console_output failed: %d", ret)
	}
	return nil
}

func (vm *vmcontext) AddVSockPort(port uint32, path string) error {
	if vm.lib.AddVsockPort == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vm.lib.AddVsockPort(vm.ctxId, port, path, true)
	if ret != 0 {
		return fmt.Errorf("krun_add_vsock_port failed: %d", ret)
	}
	return nil
}

func (vm *vmcontext) AddVirtiofs(tag, path string) error {
	if vm.lib.AddVirtiofs == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vm.lib.AddVirtiofs(vm.ctxId, tag, path)
	if ret != 0 {
		return fmt.Errorf("krun_add_virtio_fs failed: %d", ret)
	}
	return nil
}

func (vm *vmcontext) AddDisk(blockID, path string, readonly bool) error {
	if vm.lib.AddDisk == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vm.lib.AddDisk(vm.ctxId, blockID, path, readonly)
	if ret != 0 {
		return fmt.Errorf("krun_add_disk failed: %d", ret)
	}
	return nil
}

func (vm *vmcontext) Start() error {
	if vm.lib.StartEnter == nil {
		return fmt.Errorf("libkrun not loaded")
	}
	ret := vm.lib.StartEnter(vm.ctxId)
	if ret != 0 {
		return fmt.Errorf("krun_start_enter failed: %d", ret)
	}
	return nil
}

func (vm *vmcontext) Shutdown() error {
	if vm.ctxId == 0 {
		return nil
	}
	ret := vm.lib.FreeCtx(vm.ctxId)
	if ret != 0 {
		return fmt.Errorf("krun_free_ctx failed: %d", ret)
	}
	vm.ctxId = 0
	return nil
}

func (vm *vmcontext) cString(a string) unsafe.Pointer {
	if a == "" {
		return nil
	}
	if !strings.HasSuffix(a, "\000") {
		a += "\000"
	}
	b := []byte(a)
	vm.passedDown = append(vm.passedDown, b)
	return unsafe.Pointer(unsafe.SliceData(b))
}

func (vm *vmcontext) cStringArray(a []string) unsafe.Pointer {
	if len(a) == 0 {
		return nil
	}
	o := make([]unsafe.Pointer, len(a)+1, len(a)+1)
	o[len(a)] = nil // Null-terminate the array
	for i := range a {
		o[i] = vm.cString(a[i])
	}
	return unsafe.Pointer(unsafe.SliceData(o))
}

type libkrun struct {
	SetLogLevel        func(level uint32) int32                                                               `C:"krun_set_log_level"`
	InitLog            func(fd uintptr, level uint32, style uint32, options uint32) int32                     `C:"krun_init_log"`
	CreateCtx          func() int32                                                                           `C:"krun_create_ctx"`
	FreeCtx            func(ctxId uint32) int32                                                               `C:"krun_free_ctx"`
	SetVmConfig        func(ctxId uint32, cpu uint8, ram uint32) int32                                        `C:"krun_set_vm_config"`
	SetKernel          func(ctxId uint32, path string, format uint32, initramfs string, cmdline string) int32 `C:"krun_set_kernel"`
	SetExec            func(ctxId uint32, path string, args unsafe.Pointer, env unsafe.Pointer) int32         `C:"krun_set_exec"`
	SetConsoleOutput   func(ctxId uint32, path string) int32                                                  `C:"krun_set_console_output"`
	StartEnter         func(ctxId uint32) int32                                                               `C:"krun_start_enter"`
	AddVsockPort       func(ctxId, port uint32, path string, listen bool) int32                               `C:"krun_add_vsock_port2"`
	AddVirtiofs        func(ctxId uint32, tag, path string) int32                                             `C:"krun_add_virtiofs"`
	GetShutdownEventfd func(ctxId uint32) int32                                                               `C:"krun_get_shutdown_eventfd"`
	SetGpuOptions      func(ctxId, flag uint32) int32                                                         `C:"krun_set_gpu_options"`
	SetGvproxyPath     func(ctxId uint32, path string) int32                                                  `C:"krun_set_gvproxy_path"`
	SetNetMac          func(ctxId uint32, mac []uint8) int32                                                  `C:"krun_set_net_mac"`
	AddDisk            func(ctxId uint32, blockId, path string, readonly bool) int32                          `C:"krun_add_disk"`

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
