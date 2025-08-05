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
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/containerd/ttrpc"

	"github.com/dmcgowan/nerdbox/internal/ttrpcutil"
)

func (s *service) startVM(ctx context.Context, root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm != nil {
		return nil
	}

	// TODO: Get these from a configuration
	ep, err := exec.LookPath("run_vminitd")
	if err != nil {
		log.Fatal("Failed to find run_vminitd in PATH:", err)
	}
	vmlinux, err := lookupFile("vmlinux")
	if err != nil {
		log.Fatal("Failed to find vmlinux in PATH:", err)
	}
	initrd, err := lookupFile("init.img")
	if err != nil {
		log.Fatal("Failed to find init.img in PATH:", err)
	}

	f := filepath.Join(root, "run_vminitd.sock")
	if _, err := os.Stat(f); err == nil {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			log.Fatal("Failed to remove old socket file:", err)
		}
		log.Println("Removed old socket file:", f)
	} else if !os.IsNotExist(err) {
		log.Fatal(err)
	}

	args := []string{
		"-l", f,
		vmlinux,
		initrd,
	}

	cmd := exec.CommandContext(ctx, ep, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start VM: %w", err)
	}

	errC := make(chan error)
	go func() {
		defer close(errC)
		if err := cmd.Wait(); err != nil {
			errC <- err
		}
	}()

	s.shutdown.RegisterCallback(func(context.Context) error {
		log.Println("VM shutdown initiated")
		cerr := cmd.Cancel()
		select {
		case err := <-errC:
			if err != nil {
				return fmt.Errorf("failure running vm: %w", err)
			}
		default:
		}
		return cerr
	})

	var conn net.Conn
	d := 2 * time.Millisecond
	for {
		time.Sleep(time.Millisecond)
		if _, err := os.Stat(f); err == nil {
			conn, err = net.Dial("unix", f)
			if err != nil {
				return fmt.Errorf("failed to connect to TTRPC server: %w", err)
			}
			if err := ttrpcutil.PingTTRPC(conn, d); err != nil {
				conn.Close()
				d = d + time.Millisecond
				continue
			}

			s.shutdown.RegisterCallback(func(context.Context) error {
				return conn.Close()
			})
			break
		}
	}

	s.vm = &vmProcess{
		pid:    cmd.Process.Pid,
		path:   f,
		client: ttrpc.NewClient(conn),
	}
	return nil
}

func lookupFile(file string) (string, error) {
	path := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			// Unix shell semantics: path element "" means "."
			dir = "."
		}
		path := filepath.Join(dir, file)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", os.ErrNotExist
}
