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

package task

import (
	"context"
	"fmt"
	"strconv"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/vm"
)

const (
	resourcesAnnotation = "io.containerd.nerdbox.resources"
	cpuAnnotation       = resourcesAnnotation + ".cpu"
	memAnnotation       = resourcesAnnotation + ".memory"
)

type resourceConfig struct {
	cpu uint8
	mem uint32
}

func (r *resourceConfig) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	// Set default values.
	r.cpu = 2
	r.mem = 2048

	if b.Spec.Annotations == nil {
		return nil
	}

	for annotKey, annotValue := range b.Spec.Annotations {
		switch annotKey {
		case cpuAnnotation:
			cpu, err := strconv.ParseUint(annotValue, 10, 8)
			if err != nil {
				return fmt.Errorf("failed to parse %s=%s: %w", annotKey, annotValue, err)
			}
			r.cpu = uint8(cpu)
			delete(b.Spec.Annotations, annotKey)
		case memAnnotation:
			mem, err := strconv.ParseUint(annotValue, 10, 32)
			if err != nil {
				return fmt.Errorf("failed to parse %s=%s: %w", annotKey, annotValue, err)
			}
			r.mem = uint32(mem)
			delete(b.Spec.Annotations, annotKey)
		}
	}

	if r.cpu == 0 || r.mem == 0 {
		return fmt.Errorf("cpu and memory must be greater than 0")
	}

	return nil
}

func (r *resourceConfig) SetupVM(ctx context.Context, vmi vm.Instance) error {
	if err := vmi.SetCPUAndMemory(ctx, r.cpu, r.mem); err != nil {
		return fmt.Errorf("failed to apply VM resources configuration: %w", err)
	}
	return nil
}
