//go:build darwin

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
	"io"
)

// startCopy starts the host-side stdio copy for forwardIO.
//
// On darwin FIFOs must not go through the blocking file descriptors
// containerd/fifo hands out: they are kept off the runtime poller, so a
// goroutine parked in read(2) or write(2) pins an OS thread that neither Close
// nor ioShutdown can ever release. copyStreamsPolled uses descriptors that are
// registered with the poller instead, and returns an abort function for
// ioShutdown's force-close. See io_fifo_poller.go for the details.
func startCopy(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) (stdinEOF func() error, stdinDone <-chan struct{}, abort func(), err error) {
	return copyStreamsPolled(ctx, streams, stdin, stdout, stderr, done)
}
