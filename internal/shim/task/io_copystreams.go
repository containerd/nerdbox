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

	"github.com/containerd/log"
)

// copyStdinUntilClose reads from f and writes raw bytes to sc until closeCh
// is closed (CloseIO) or f delivers EOF. On either exit it calls
// sc.CloseWrite() to send OP_SHUTDOWN(SEND) in-order on the vsock stdin
// stream, guaranteeing the guest sees EOF after all data already written —
// not via an out-of-band RPC that could race in-flight bytes.
func copyStdinUntilClose(ctx context.Context, sc interface {
	io.Writer
	CloseWrite() error
}, f io.Reader, buf []byte, closeCh <-chan struct{}) {
	type readResult struct {
		n   int
		err error
	}
	readCh := make(chan readResult, 1)
	for {
		go func() {
			n, err := f.Read(buf)
			readCh <- readResult{n, err}
		}()
		select {
		case <-closeCh:
			// CloseIO fired: drain the pending read then send in-band EOF.
			res := <-readCh
			for {
				if res.n > 0 {
					if _, err := sc.Write(buf[:res.n]); err != nil {
						log.G(ctx).WithError(err).Warn("error writing stdin on CloseIO")
						break
					}
				}
				if res.err != nil || res.n == 0 {
					// EOF (client closed its write end) or a read error:
					// nothing left to drain.
					break
				}

				res.n, res.err = f.Read(buf)
			}
			if err := sc.CloseWrite(); err != nil {
				log.G(ctx).WithError(err).Warn("error sending stdin EOF via CloseWrite")
			}
			return
		case res := <-readCh:
			if res.n > 0 {
				if _, err := sc.Write(buf[:res.n]); err != nil {
					log.G(ctx).WithError(err).Warn("error writing stdin")
					return
				}
			}
			if res.err != nil {
				// Pipe/named-pipe EOF: client closed its write end.
				if err := sc.CloseWrite(); err != nil {
					log.G(ctx).WithError(err).Warn("error sending stdin EOF on pipe close")
				}
				return
			}
		}
	}
}
