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
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
)

func setupMounts(m []*types.Mount, root string) ([]*types.Mount, bool, error) {
	if len(m) == 1 && (m[0].Type == "overlay" || m[0].Type == "bind") {
		mnt := mount.Mount{
			Type:    m[0].Type,
			Source:  m[0].Source,
			Options: m[0].Options,
		}
		if err := mnt.Mount(root); err != nil {
			return nil, false, err
		}
		/*
			return []*types.Mount{&types.Mount{
				Type:   "bind",
				Source: "/root/rootfs",
				// TODO: Translate the options
				//Options: m[0].Options,
			}}, true, nil
		*/
		return []*types.Mount{}, true, nil
	}
	// TODO: Handle other mount types and devices

	return m, false, nil
}
