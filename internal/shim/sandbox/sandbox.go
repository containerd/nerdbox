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

// Package sandbox defines the interface for managing sandbox instances
package sandbox

import (
	"context"
	"net"

	"github.com/containerd/ttrpc"
)

// Sandbox defines the interface for creating and managing the sandbox
// Only a single instance is allowed, calling Start with a sandbox already created
// or calling Client or Stop without a created sandbox will return a precondition error.
type Sandbox interface {
	Start(context.Context, ...Opt) error
	Stop(context.Context) error
	Client() (*ttrpc.Client, error)
	StartStream(context.Context, string) (net.Conn, error)
}

type Filesystem struct {
	Tag       string
	MountPath string
	Readonly  bool
}

type DiskFlags uint8

const (
	DiskFlagReadonly DiskFlags = 1 << iota
	DiskFlagVMDK     DiskFlags = 1 << iota
)

type Disk struct {
	BlockID   string
	MountPath string
	Flags     DiskFlags
}

type NIC struct {
	Endpoint string
	MAC      net.HardwareAddr
	Mode     int
	Features uint32
	Flags    uint32
}

type Options struct {
	Filesystems []Filesystem
	Disks       []Disk
	NICs        []NIC
	StateDir    string
	InitArgs    []string
	CPU         uint8
	Memory      uint32 // in MiB
}

type Opt func(*Options)

func WithFS(tag, mountPath string, readonly bool) Opt {
	return func(o *Options) {
		o.Filesystems = append(o.Filesystems, Filesystem{
			Tag:       tag,
			MountPath: mountPath,
			Readonly:  readonly,
		})
	}
}

func WithDisk(blockID, mountPath string, flags DiskFlags) Opt {
	return func(o *Options) {
		o.Disks = append(o.Disks, Disk{
			BlockID:   blockID,
			MountPath: mountPath,
			Flags:     flags,
		})
	}
}

func WithNIC(endpoint string, mac net.HardwareAddr, mode int, features, flags uint32) Opt {
	return func(o *Options) {
		o.NICs = append(o.NICs, NIC{
			Endpoint: endpoint,
			MAC:      mac,
			Mode:     mode,
			Features: features,
			Flags:    flags,
		})
	}
}

func WithStateDir(dir string) Opt {
	return func(o *Options) {
		o.StateDir = dir
	}
}

func WithInitArgs(args ...string) Opt {
	return func(o *Options) {
		o.InitArgs = append(o.InitArgs, args...)
	}
}

func WithResources(cpu uint8, memory uint32) Opt {
	return func(o *Options) {
		o.CPU = cpu
		o.Memory = memory
	}
}
