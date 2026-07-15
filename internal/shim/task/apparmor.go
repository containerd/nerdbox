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

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

// clearApparmorProfile strips Process.ApparmorProfile from the incoming OCI
// spec. CRI sets this field to the name of an AppArmor profile loaded on
// the host (e.g. via a pod's appArmorProfile field or the deprecated
// container.apparmor.security.beta.kubernetes.io annotation). That name is
// meaningless inside the VM guest: the guest kernel may not have AppArmor
// enabled at all, and even if it does, it never loaded a profile by that
// name. Left unmodified, the guest's crun invocation fails outright trying
// to apply an unknown profile. Clearing the field runs the container
// unconfined by AppArmor inside the guest, which is consistent with how
// this shim already handles other host-specific confinement it cannot
// honor in a nested kernel (see sanitizeNamespaces for the equivalent
// treatment of host namespace paths).
func clearApparmorProfile(_ context.Context, b *bundle.Bundle) error {
	if b.Spec.Process != nil {
		b.Spec.Process.ApparmorProfile = ""
	}
	return nil
}
