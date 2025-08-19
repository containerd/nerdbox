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

package systools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containerd/log"
)

// DumpInfo dumps information about the system
func DumpInfo(ctx context.Context) {
	filepath.Walk("/", func(path string, info os.FileInfo, err error) error {
		if path == "/proc" || path == "/sys" {
			path = fmt.Sprintf("%s (skipping)", path)
			err = filepath.SkipDir
		}

		if info != nil {
			log.G(ctx).WithFields(
				log.Fields{
					"mode": info.Mode(),
					"size": info.Size(),
				}).Debug(path)
		}

		return err
	})

	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to read kernel command line")
	} else {
		log.G(ctx).WithField("cmdline", string(b)).Debug("kernel command line")
	}
	log.G(ctx).WithField("ncpu", runtime.NumCPU()).Debug("Runtime info")

	if b, err := exec.CommandContext(ctx, "/sbin/crun", "--version").Output(); err != nil {
		log.G(ctx).WithError(err).Error("failed to get crun version")
	} else {
		log.G(ctx).WithField("command", "crun --version").Debug(strings.ReplaceAll(string(b), "\n", ", "))
	}
	DumpPids(ctx)
}

func DumpFile(ctx context.Context, name string) {
	e := log.G(ctx)
	if e.Logger.IsLevelEnabled(log.DebugLevel) {
		f, err := os.Open(name)
		if err == nil {
			defer f.Close()
			log.G(ctx).WithField("f", name).Debug("dumping file to stderr")
			if strings.HasSuffix(name, ".json") {
				var b bytes.Buffer
				v := map[string]any{}
				io.Copy(&b, f)
				if err := json.Unmarshal(b.Bytes(), &v); err != nil {
					os.Stderr.Write(b.Bytes())
					fmt.Fprintln(os.Stderr)
					return
				}
				enc := json.NewEncoder(os.Stderr)
				enc.SetIndent("", "  ")
				enc.Encode(v)
			} else {
				io.Copy(os.Stderr, f)
				fmt.Fprintln(os.Stderr)
			}
		} else {
			log.G(ctx).WithError(err).WithField("f", name).Warn("failed to open file to dump")
		}
	}
}
