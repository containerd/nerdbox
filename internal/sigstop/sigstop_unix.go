//go:build unix

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

package sigstop

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/containerd/log"
)

func Raise() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR2)
	defer signal.Stop(sigCh)

	log.G(context.Background()).Debug("Waiting for SIGUSR2")
	if err := syscall.Kill(os.Getpid(), syscall.SIGSTOP); err != nil {
		panic(err)
	}

	<-sigCh
	log.G(context.Background()).Debug("SIGUSR2 received. Resuming process.")
}
