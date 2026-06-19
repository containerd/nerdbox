//go:build windows

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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/containerd/log"
)

func copyStreams(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) error {
	var cwg sync.WaitGroup
	var copying atomic.Int32
	copying.Store(2)
	var sameFile *countingWriteCloser
	for _, i := range []struct {
		name string
		dest func(wc io.WriteCloser, rc io.Closer)
	}{
		{
			name: stdout,
			dest: func(wc io.WriteCloser, rc io.Closer) {
				cwg.Add(1)
				go func() {
					cwg.Done()
					p := bufPool.Get().(*[]byte)
					defer bufPool.Put(p)
					if _, err := io.CopyBuffer(wc, streams[1], *p); err != nil {
						log.G(ctx).WithError(err).WithField("stream", streams[1]).Warn("error copying stdout")
					}
					if copying.Add(-1) == 0 {
						close(done)
					}
					wc.Close()
					if rc != nil {
						rc.Close()
					}
				}()
			},
		}, {
			name: stderr,
			dest: func(wc io.WriteCloser, rc io.Closer) {
				cwg.Add(1)
				go func() {
					cwg.Done()
					p := bufPool.Get().(*[]byte)
					defer bufPool.Put(p)
					if _, err := io.CopyBuffer(wc, streams[2], *p); err != nil {
						log.G(ctx).WithError(err).Warn("error copying stderr")
					}
					if copying.Add(-1) == 0 {
						close(done)
					}
					wc.Close()
					if rc != nil {
						rc.Close()
					}
				}()
			},
		},
	} {
		if i.name == "" {
			if copying.Add(-1) == 0 {
				close(done)
			}
			continue
		}

		var (
			fw  io.WriteCloser
			err error
		)

		// On Windows, check if the path is a named pipe (\\.\pipe\...).
		// Otherwise, fall back to regular file I/O.
		if isNamedPipe(i.name) {
			fw, err = dialPipeWaiting(ctx, i.name)
			if err != nil {
				return fmt.Errorf("containerd-shim: connecting to named pipe %q failed: %w", i.name, err)
			}
		} else {
			if sameFile != nil {
				sameFile.bumpCount(1)
				i.dest(sameFile, nil)
				continue
			}
			if fw, err = os.OpenFile(i.name, os.O_WRONLY|os.O_APPEND, 0); err != nil {
				return fmt.Errorf("containerd-shim: opening file %q failed: %w", i.name, err)
			}
			if stdout == stderr {
				sameFile = newCountingWriteCloser(fw, 1)
			}
		}
		i.dest(fw, nil)
	}
	if stdin != "" {
		var f io.ReadCloser
		if isNamedPipe(stdin) {
			conn, err := dialPipeWaiting(ctx, stdin)
			if err != nil {
				return fmt.Errorf("containerd-shim: connecting to named pipe %q for stdin failed: %w", stdin, err)
			}
			f = conn
		} else {
			var err error
			f, err = os.Open(stdin)
			if err != nil {
				return fmt.Errorf("containerd-shim: opening %s failed: %s", stdin, err)
			}
		}
		cwg.Add(1)
		go func() {
			cwg.Done()
			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)

			io.CopyBuffer(streams[0], f, *p)
			streams[0].Close()
			f.Close()
		}()
	}
	cwg.Wait()
	return nil
}

// isNamedPipe checks if a path looks like a Windows named pipe (\\.\pipe\...).
func isNamedPipe(path string) bool {
	return len(path) > 9 && path[:9] == `\\.\pipe\`
}

// dialPipeWaiting dials the named pipe at path, waiting for the pipe to be
// created if it does not exist yet. winio.DialPipe already retries while the
// pipe exists but all server instances are busy (ERROR_PIPE_BUSY); it fails
// immediately if the pipe does not exist (ERROR_FILE_NOT_FOUND). For client-owned
// process I/O, the consumer is the pipe server and may not have called ListenPipe
// yet when the shim binds stdio, so we also poll through "not found" until
// ctx is cancelled or pipeConnectTimeout elapses.
func dialPipeWaiting(ctx context.Context, path string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, pipeConnectTimeout)
	defer cancel()
	for {
		// DialPipeContext honors ctx promptly — including while it retries a
		// busy pipe (ERROR_PIPE_BUSY) — so a caller's cancellation or deadline
		// is observed within winio's 10ms retry granularity rather than after a
		// fixed per-dial timeout. It returns immediately when the pipe does not
		// exist yet (ERROR_FILE_NOT_FOUND), which we treat as "not ready" and
		// poll below.
		conn, err := winio.DialPipeContext(ctx, path)
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pipeNotFoundPoll):
		}
	}
}

// pipeConnectTimeout bounds the overall connect: waiting both for a busy pipe's
// server instance to free up (ERROR_PIPE_BUSY) and for a not-yet-created pipe
// server to come up (ERROR_FILE_NOT_FOUND).
var pipeConnectTimeout = 30 * time.Second

// pipeNotFoundPoll is the backoff between dial attempts while the pipe does
// not yet exist.
const pipeNotFoundPoll = 20 * time.Millisecond
