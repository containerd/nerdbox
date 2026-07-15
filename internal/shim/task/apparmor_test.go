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
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func TestClearApparmorProfile(t *testing.T) {
	t.Run("nil Process is a no-op", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{}}
		require.NoError(t, clearApparmorProfile(context.Background(), b))
		assert.Nil(t, b.Spec.Process)
	})

	t.Run("clears a host AppArmor profile", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{
			Process: &specs.Process{ApparmorProfile: "docker-default"},
		}}
		require.NoError(t, clearApparmorProfile(context.Background(), b))
		assert.Empty(t, b.Spec.Process.ApparmorProfile)
	})

	t.Run("no-op when no profile was set", func(t *testing.T) {
		b := &bundle.Bundle{Spec: specs.Spec{Process: &specs.Process{}}}
		require.NoError(t, clearApparmorProfile(context.Background(), b))
		assert.Empty(t, b.Spec.Process.ApparmorProfile)
	})
}
