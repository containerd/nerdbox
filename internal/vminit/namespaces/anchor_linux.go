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

package namespaces

import (
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/unix"
)

// Anchor is the body of the process that holds a PID namespace open. vminitd
// re-execs itself with AnchorSubcommand to get here; see createPID for why a
// live process is required rather than a bind mount.
//
// As PID 1 of its namespace it must reap anything reparented to it, which
// happens whenever a process elsewhere in the shared namespace outlives its
// original parent. Left unreaped those would accumulate as zombies for the
// sandbox's whole lifetime.
//
// Every signal is ignored. PID 1 of a namespace is already protected from
// signals with default dispositions sent from inside that namespace, but not
// from those sent by an ancestor namespace, so installing an explicit no-op
// handler is what actually makes it unkillable by anything except SIGKILL —
// which is how Manager.Delete tears the namespace down.
//
// Anchor never returns.
func Anchor() {
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
			// Reaped one; check immediately for more.
		case unix.ECHILD:
			// Nothing to reap right now. Sleep rather than spin until
			// something is reparented here.
			time.Sleep(500 * time.Millisecond)
		default:
			time.Sleep(time.Second)
		}
	}
}
