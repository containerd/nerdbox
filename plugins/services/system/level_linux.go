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

package version

import (
	"log/slog"

	"github.com/containerd/nerdbox/pkg/vminit/initd"
)

// setSlogLevel updates the vminitd slog handler level via the exported
// initd.LogLevel LevelVar. Only meaningful on Linux where vminitd runs.
func setSlogLevel(level slog.Level) {
	initd.LogLevel.Set(level)
}
