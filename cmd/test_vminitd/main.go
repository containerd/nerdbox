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

package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	ctx := context.Background()

	// TODO: Make all of these configurable

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

	f := "./run_vminitd.sock"
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

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)

	errC := make(chan error)
	go func() {
		defer close(errC)
		if err := cmd.Run(); err != nil {
			errC <- err
		}
	}()

	select {
	case err := <-errC:
		if err != nil {
			log.Fatal("Failed to start vm:", err)
		}
	case <-sigC:
		if err := cmd.Cancel(); err != nil {
			log.Fatal("Failed to shutdown context:", err)
		}
	}
	log.Println("VM shutdown initiated")
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
