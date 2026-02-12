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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
)

type streamCreator interface {
	StartStream(ctx context.Context, streamID string) (net.Conn, error)
}

// generateStreamID creates a pseudo-random stream ID with the given prefix.
// The format is "{prefix}-{nanosecond}-{random}" to minimize collisions.
func generateStreamID(prefix string) string {
	var b [4]byte
	rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), base64.RawURLEncoding.EncodeToString(b[:]))
}

func (s *service) forwardIO(ctx context.Context, ss streamCreator, idPrefix string, sio stdio.Stdio) (stdio.Stdio, func(ctx context.Context) error, error) {
	pio := sio
	if pio.IsNull() {
		return pio, nil, nil
	}
	u, err := url.Parse(pio.Stdout)
	if err != nil {
		return stdio.Stdio{}, nil, fmt.Errorf("unable to parse stdout uri: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}
	var streams [3]io.ReadWriteCloser
	switch u.Scheme {
	case "stream":
		// Pass through
		return pio, nil, nil
	case "fifo", "pipe":
		pio, streams, err = createStreams(ctx, ss, idPrefix, pio)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}

		//pio.io, err = runc.NewPipeIO(ioUID, ioGID, withConditionalIO(stdio))
	case "file":
		filePath := u.Path
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return stdio.Stdio{}, nil, err
		}
		var f *os.File
		f, err = os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
		f.Close()
		pio.Stdout = filePath
		pio.Stderr = filePath
		pio, streams, err = createStreams(ctx, ss, idPrefix, pio)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
	default:
		// TODO: Support "binary"
		return stdio.Stdio{}, nil, fmt.Errorf("unsupported STDIO scheme %s: %w", u.Scheme, errdefs.ErrNotImplemented)
	}
	if err != nil {
		return stdio.Stdio{}, nil, err
	}

	defer func() {
		if err != nil {
			for i, c := range streams {
				if c != nil && (i != 2 || c != streams[1]) {
					c.Close()
				}
			}
		}
	}()
	ioDone := make(chan struct{})
	if err = copyStreams(ctx, streams, sio.Stdin, sio.Stdout, sio.Stderr, ioDone); err != nil {
		return stdio.Stdio{}, nil, err
	}
	return pio, func(ctx context.Context) error {
		for i, c := range streams {
			if c != nil && (i != 2 || c != streams[1]) {
				c.Close()
			}
		}
		select {
		case <-ioDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil
}

func createStreams(ctx context.Context, ss streamCreator, idPrefix string, io stdio.Stdio) (_ stdio.Stdio, conns [3]io.ReadWriteCloser, err error) {
	defer func() {
		if err != nil {
			for i, c := range conns {
				if c != nil && (i != 2 || c != conns[1]) {
					c.Close()
				}
			}
		}
	}()
	if io.Stdin != "" {
		sid := generateStreamID(idPrefix + "-stdin")
		conn, err := ss.StartStream(ctx, sid)
		if err != nil {
			return io, conns, fmt.Errorf("failed to start stdin stream: %w", err)
		}
		io.Stdin = fmt.Sprintf("stream://%s", sid)
		conns[0] = conn
	}

	stdout := io.Stdout
	if stdout != "" {
		sid := generateStreamID(idPrefix + "-stdout")
		conn, err := ss.StartStream(ctx, sid)
		if err != nil {
			return io, conns, fmt.Errorf("failed to start stdout stream: %w", err)
		}
		io.Stdout = fmt.Sprintf("stream://%s", sid)
		conns[1] = conn
	}

	if io.Stderr != "" {
		if io.Stderr == stdout {
			io.Stderr = io.Stdout
			conns[2] = conns[1]
		} else {
			sid := generateStreamID(idPrefix + "-stderr")
			conn, err := ss.StartStream(ctx, sid)
			if err != nil {
				return io, conns, fmt.Errorf("failed to start stderr stream: %w", err)
			}
			io.Stderr = fmt.Sprintf("stream://%s", sid)
			conns[2] = conn
		}
	}
	return io, conns, nil
}

var bufPool = sync.Pool{
	New: func() interface{} {
		// setting to 4096 to align with PIPE_BUF
		// http://man7.org/linux/man-pages/man7/pipe.7.html
		buffer := make([]byte, 4096)
		return &buffer
	},
}

// countingWriteCloser masks io.Closer() until close has been invoked a certain number of times.
type countingWriteCloser struct {
	io.WriteCloser
	count atomic.Int64
}

func newCountingWriteCloser(c io.WriteCloser, count int64) *countingWriteCloser {
	cwc := &countingWriteCloser{
		c,
		atomic.Int64{},
	}
	cwc.bumpCount(count)
	return cwc
}

func (c *countingWriteCloser) bumpCount(delta int64) int64 {
	return c.count.Add(delta)
}

func (c *countingWriteCloser) Close() error {
	if c.bumpCount(-1) > 0 {
		return nil
	}
	return c.WriteCloser.Close()
}
