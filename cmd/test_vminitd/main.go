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
	"os/signal"
	"syscall"

	"github.com/dmcgowan/nerdbox/internal/vm/runvm"
)

func main() {
	ctx := context.Background()

	vm, err := runvm.NewVMInstance()
	if err != nil {
		log.Fatal("Failed to create VM instance:", err)
	}

	// TODO: Make the socket configurable
	if err := vm.Start(ctx, "./run_vminitd.sock", ""); err != nil {
		log.Fatal("Failed to start VM instance:", err)
	}

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)

	<-sigC

	log.Println("VM shutdown initiated")
	if err := vm.Shutdown(ctx); err != nil {
		log.Fatal("Failed to shutdown context:", err)
	}
}
