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

// Package podpause implements vminitd's hidden "pod-pause" subcommand: a
// minimal anchor process that exists purely to give a sandbox's shared PID
// namespace a persistent PID 1.
//
// A Linux PID namespace has no content of its own and is torn down (every
// process in it killed) the moment its PID 1 exits — unlike a network or
// IPC namespace, which can be anchored by a bind-mount alone with no
// process required. See internal/vminit/podns, which execs this
// subcommand with CLONE_NEWPID to create the namespace in the first
// place.
package podpause

import (
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/unix"
)

// Run is the body of the pod-pause process. It never expects to have
// functional children in the ordinary sense, but as PID 1 of its
// namespace, it is responsible for reaping any process that ends up
// reparented to it — which happens whenever a process's original parent
// (elsewhere in the shared PID namespace) exits before it does. Without
// reaping them, those processes would persist as zombies for the
// sandbox's entire lifetime.
//
// All signals are ignored: PID 1 of a namespace never applies a default
// disposition to a signal it hasn't installed a handler for (see
// signal(7)), so an unhandled signal delivered here would otherwise be
// silently dropped anyway, but installing an explicit no-op handler is
// what actually keeps it that way defensively. The only thing that can
// terminate this process is an unblockable SIGKILL, which is what the
// host uses to tear the namespace down when the sandbox stops.
//
// Run never returns.
func Run() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh)
	go func() {
		for range sigCh {
			// Ignore everything.
		}
	}()

	for {
		var ws unix.WaitStatus
		_, err := unix.Wait4(-1, &ws, 0, nil)
		switch err {
		case nil:
			// Reaped a child; immediately check for more.
		case unix.ECHILD:
			// No children currently exist to reap. Sleep briefly rather
			// than spinning until one is reparented here.
			time.Sleep(500 * time.Millisecond)
		default:
			time.Sleep(time.Second)
		}
	}
}
