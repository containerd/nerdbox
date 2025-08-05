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

package integration

import (
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/ttrpc"

	"github.com/dmcgowan/nerdbox/internal/ttrpcutil"
)

func resolvePath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(absPath); err != nil {
		return "", err
	}
	return absPath, nil
}

var (
	runVminitdPath string
	vmlinuxPath    string
	initImgPath    string
)

func TestMain(m *testing.M) {
	var err error
	runVminitdPath, err = resolvePath("../build/run_vminitd")
	if err != nil {
		log.Fatalf("Failed to resolve run_vminitd path: %v", err)
	}

	r := m.Run()

	os.Exit(r)
}

func startVM(t *testing.T) *ttrpc.Client {
	f := filepath.Join(t.TempDir(), "run_vminitd.sock")

	args := []string{
		"-l", f,
		vmlinuxPath,
		initImgPath,
	}

	t1 := time.Now()
	cmd := exec.CommandContext(t.Context(), runVminitdPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start run_vminitd: %v", err)
	}
	t.Log("Started run_vminitd with pid", cmd.Process.Pid)

	t.Cleanup(func() {
		cmd.Cancel()
	})

	var conn net.Conn
	d := 2 * time.Millisecond
	for {
		time.Sleep(time.Millisecond)
		if _, err := os.Stat(f); err == nil {
			conn, err = net.Dial("unix", f)
			if err != nil {
				t.Fatalf("Failed to connect to TTRPC server: %v", err)
			}
			t2 := time.Now()
			if err := ttrpcutil.PingTTRPC(conn, d); err != nil {
				t.Logf("Failed to ping TTRPC server (%s): %v", err, time.Since(t2))
				conn.Close()
				d = d + time.Millisecond
				continue
			}
			t.Log("Successfully pinged TTRPC server at", f, "after", time.Since(t2))

			t.Cleanup(func() {
				conn.Close()
			})
			break
		}
	}
	t.Log("Connected to TTRPC server at", f, "after", time.Since(t1))

	return ttrpc.NewClient(conn)
}
