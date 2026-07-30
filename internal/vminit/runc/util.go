//go:build linux

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

package runc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// ShouldKillAllOnExit reads the bundle's OCI spec and reports whether the
// container's other processes need to be killed explicitly when its init
// process exits.
//
// It returns false only when the spec has a PID namespace entry with an
// empty Path — the container's own, private PID namespace (the default,
// and PID namespace sharing's opposite: see internal/shim/task/namespaces.go
// on the host side). In that case nothing else is needed: the kernel tears
// the namespace down and kills every process still in it the moment PID 1
// exits, so by the time this is even checked, everything else already is
// gone.
//
// It returns true otherwise — including a shared PID namespace (a
// non-empty Path), no PID namespace entry in the spec at all (the host's
// PID namespace, which the kernel will not tear down on this container's
// account), and a failure to read the spec, which is treated as the safe
// default rather than silently skipping cleanup.
func ShouldKillAllOnExit(ctx context.Context, bundlePath string) bool {
	spec, err := readSpec(bundlePath)
	if err != nil {
		log.G(ctx).WithError(err).Error("shouldKillAllOnExit: failed to read config.json")
		return true
	}

	if spec.Linux != nil {
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type == specs.PIDNamespace && ns.Path == "" {
				return false
			}
		}
	}
	return true
}

func readSpec(p string) (*specs.Spec, error) {
	const configFileName = "config.json"
	f, err := os.Open(filepath.Join(p, configFileName))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var s specs.Spec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}
