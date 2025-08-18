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
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/log"
)

func DumpPids(ctx context.Context) {
	log.G(ctx).Debug("Dumping /proc/pid info")

	es, err := os.ReadDir("/proc")
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to read /proc")
		return
	}
	if len(es) == 0 {
		log.G(ctx).Infof("no files in /proc")
	}
	for _, e := range es {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			dumpProcPid(ctx, e.Name())
		}
	}
}

func dumpProcPid(ctx context.Context, pid string) {
	f, err := os.Open(filepath.Join("/proc", pid, "status"))
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to open /proc/%s/status", pid)
		return
	}
	defer f.Close()

	values := make(map[string]string)
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, found := strings.Cut(s.Text(), ":")
		if !found {
			continue
		}
		values[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}

	fields := log.Fields{
		"pid":  pid,
		"ppid": values["ppid"],
	}
	if vmrss, ok := values["vmrss"]; ok && vmrss != "" {
		fields["vmrss"] = vmrss
	}

	b, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
	if err == nil && len(b) > 0 {
		fields["cmdline"] = strings.ReplaceAll(strings.TrimSpace(string(b)), "\x00", " ")
	}

	log.G(ctx).WithFields(fields).Debug(values["name"])
}
