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
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/containerd/log"
)

// stdinStreamWriteCloser mirrors the Unix definition; both builds need it for
// the CloseIO-driven in-band stdin EOF.
type stdinStreamWriteCloser interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// writeErrRecorder wraps the guest-facing stdin stream so the redial loop
// below can tell the two failure modes of io.CopyBuffer apart. A read-side
// error (including the ordinary nil-error EOF) means the *client*
// disconnected, which is a detach candidate and should be followed by a
// reconnect. A write-side error means the stream to the guest is broken, so
// no future client could ever deliver bytes and reconnecting would spin
// forever without closing stdinDone.
//
// It is only ever written by copyStreams' single stdin goroutine, so err
// needs no synchronization. Embedding io.Writer (rather than the full
// stdinStreamWriteCloser) also hides any ReaderFrom the underlying stream
// may implement, keeping io.CopyBuffer on the path that routes every byte
// through Write and therefore through this recorder.
type writeErrRecorder struct {
	io.Writer
	err error
}

func (w *writeErrRecorder) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

// copyStreams returns a stdinEOF function and a stdinDone channel for the
// stdin pipe (both nil when stdin is empty).
//
// For a named pipe, stdinEOF (invoked by CloseIO or container teardown)
// signals the copy goroutine that the next client disconnect is real EOF
// rather than a detach. Unlike a POSIX FIFO, a Windows named pipe
// connection has no notion of holding a second, independent writer
// reference to keep the "conversation" from ending when the client
// disconnects. Instead, this mirrors the same client-detach/re-attach
// contract using reconnection: go-winio's ListenPipe (matching
// containerd's own stdio server model, where the caller is the named-pipe
// server and the shim is the client) creates a fresh pipe instance for
// every Accept, so a new client can connect to the same pipe path after a
// previous one disconnects. When the current connection ends without
// stdinEOF having been called, the goroutine treats it as a detach and
// dials the same pipe path again, waiting (with no timeout, only
// cancelled by stdinEOF or container teardown) for a new client to
// reconnect before relaying any more bytes -- exactly as the POSIX side
// waits for the FIFO to reach a real EOF only after CloseIO releases its
// write reference. Only when stdinEOF has been called does the next
// disconnect (or the current one, if it already happened) deliver EOF to
// the guest via CloseWrite.
//
// Reconnecting is only correct when the copy ended because the client
// went away. If it ended because writing to the guest-facing stream
// failed, the loop stops instead -- see writeErrRecorder.
//
// A plain file (the non-named-pipe fallback) has no reconnect concept, so
// it is read once to EOF and always delivers EOF immediately; stdinEOF is
// a no-op in that case.
func copyStreams(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) (stdinEOF func() error, stdinDone <-chan struct{}, err error) {
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
						log.G(ctx).WithError(err).WithField("stream_id", streams[1]).Warn("error copying stdout")
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
			fw, err = winio.DialPipe(i.name, &pipeDialTimeout)
			if err != nil {
				return nil, nil, fmt.Errorf("containerd-shim: connecting to named pipe %q failed: %w", i.name, err)
			}
		} else {
			if sameFile != nil {
				sameFile.bumpCount(1)
				i.dest(sameFile, nil)
				continue
			}
			if fw, err = os.OpenFile(i.name, os.O_WRONLY|os.O_APPEND, 0); err != nil {
				return nil, nil, fmt.Errorf("containerd-shim: opening file %q failed: %w", i.name, err)
			}
			if stdout == stderr {
				sameFile = newCountingWriteCloser(fw, 1)
			}
		}
		i.dest(fw, nil)
	}
	if stdin != "" {
		sc, ok := streams[0].(stdinStreamWriteCloser)
		if !ok {
			return nil, nil, fmt.Errorf("stdin stream connection does not implement CloseWrite; vsock conn required")
		}

		namedPipe := isNamedPipe(stdin)

		// Establish the first connection synchronously, bounded by
		// pipeDialTimeout, preserving the existing Exec()/Create()
		// contract: if no client attaches to stdin promptly, the RPC
		// fails fast rather than hanging.
		var f io.ReadCloser
		if namedPipe {
			conn, err := winio.DialPipe(stdin, &pipeDialTimeout)
			if err != nil {
				return nil, nil, fmt.Errorf("containerd-shim: connecting to named pipe %q for stdin failed: %w", stdin, err)
			}
			f = conn
		} else {
			var err error
			f, err = os.Open(stdin)
			if err != nil {
				return nil, nil, fmt.Errorf("containerd-shim: opening %s failed: %s", stdin, err)
			}
		}

		// closeRequested is closed by stdinEOF (CloseIO or container
		// teardown) to signal that the next client disconnect -- or the
		// current one, if it has already happened -- must deliver EOF to
		// the guest instead of waiting for a new client to reconnect.
		closeRequested := make(chan struct{})
		var closeOnce sync.Once
		stdinEOF = func() error {
			closeOnce.Do(func() { close(closeRequested) })
			return nil
		}

		stdinDoneCh := make(chan struct{})
		stdinDone = stdinDoneCh
		cwg.Add(1)
		go func() {
			cwg.Done()
			defer close(stdinDoneCh)
			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)
			w := &writeErrRecorder{Writer: sc}

		readLoop:
			for {
				// Drain to a real EOF: the peer closing its write end is
				// what unblocks this read, so every byte buffered before
				// EOF is forwarded before we either reconnect or deliver
				// the in-band CloseWrite below.
				if _, err := io.CopyBuffer(w, f, *p); err != nil {
					log.G(ctx).WithError(err).Warn("error copying stdin")
				}
				f.Close()

				// A write failure means the guest-facing stream is gone,
				// not that the client detached. Reconnecting would block
				// on a client that can never be serviced, leaving
				// stdinDone open and stalling ioShutdown for its full
				// timeout, so stop the loop instead.
				if w.err != nil {
					log.G(ctx).WithError(w.err).Warn("stdin stream to guest failed; not waiting for client re-attach")
					break readLoop
				}

				if !namedPipe {
					break readLoop
				}
				select {
				case <-closeRequested:
					break readLoop
				default:
				}

				// The client disconnected without stdinEOF having been
				// called: treat this as a detach and wait for a new
				// client to reconnect on the same pipe path. This wait is
				// deliberately not time-bounded -- a detach may
				// legitimately last a long time -- and is rooted in
				// context.Background() rather than ctx, since ctx is the
				// Exec()/Create() RPC's context and is typically cancelled
				// as soon as that RPC returns. It is only cancelled by
				// stdinEOF (CloseIO, or container teardown calling
				// stdinEOF as a safety net).
				dialCtx, cancel := context.WithCancel(context.Background())
				go func() {
					select {
					case <-closeRequested:
						cancel()
					case <-dialCtx.Done():
					}
				}()
				conn, err := winio.DialPipeContext(dialCtx, stdin)
				cancel()
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						log.G(ctx).WithError(err).Warn("error reconnecting to stdin pipe after detach")
					}
					break readLoop
				}
				f = conn
			}

			if err := sc.CloseWrite(); err != nil {
				log.G(ctx).WithError(err).Warn("error sending stdin EOF via CloseWrite")
			}
		}()
	}
	cwg.Wait()
	return stdinEOF, stdinDone, nil
}

// isNamedPipe checks if a path looks like a Windows named pipe (\\.\pipe\...).
func isNamedPipe(path string) bool {
	return len(path) > 9 && path[:9] == `\\.\pipe\`
}

var pipeDialTimeout = 5 * time.Second
