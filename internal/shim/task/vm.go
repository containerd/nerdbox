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

	"github.com/dmcgowan/nerdbox/internal/vm/runvm"
)

func (s *service) startVM(ctx context.Context, socketPath string, mountPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm != nil {
		return nil
	}

	s.vm = runvm.NewVMInstance()
	return s.vm.Start(ctx, socketPath, mountPath)
}
