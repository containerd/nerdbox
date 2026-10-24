//go:build !darwin

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

// startCopy starts the host-side stdio copy for forwardIO. Everywhere but
// darwin that is copyStreams, which has no abort function: ioShutdown only
// closes the vsock streams, exactly as before. See io_copy_darwin.go.
func startCopy(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) (stdinEOF func() error, stdinDone <-chan struct{}, abort func(), err error) {
	stdinEOF, stdinDone, err = copyStreams(ctx, streams, stdin, stdout, stderr, done)
	return stdinEOF, stdinDone, nil, err
}
