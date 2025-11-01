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

package kvm

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// Obtained by running a C program with:
	//
	// 		printf("KVM_GET_API_VERSION: 0x%llX\n", KVM_GET_API_VERSION);
	//
	ioctlKVMGetAPIVersion = 0xAE00

	expectedKVMAPIVersion = 12
)

// CheckKVM checks if KVM is available on the system.
func CheckKVM() error {
	fd, err := unix.Open("/dev/kvm", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("failed to open /dev/kvm: %w. Your system may lack KVM support or you may have insufficient permissions", err)
	}
	defer syscall.Close(fd)

	// Kernel docs says:
	//
	//     Applications should refuse to run if KVM_GET_API_VERSION returns a value other than 12.
	//
	// See https://docs.kernel.org/virt/kvm/api.html#kvm-get-api-version
	apiVersion, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd), ioctlKVMGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("failed to get KVM API version: %w. You may have insufficient permissions", errno)
	}
	if apiVersion != expectedKVMAPIVersion {
		return fmt.Errorf("KVM API version mismatch; expected %d, got %d", expectedKVMAPIVersion, apiVersion)
	}

	return nil
}
