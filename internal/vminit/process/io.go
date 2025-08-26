//go:build !windows

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

package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/fifo"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/dmcgowan/nerdbox/internal/vminit/stream"
)

const binaryIOProcTermTimeout = 12 * time.Second // Give logger process solid 10 seconds for cleanup

var bufPool = sync.Pool{
	New: func() interface{} {
		// setting to 4096 to align with PIPE_BUF
		// http://man7.org/linux/man-pages/man7/pipe.7.html
		buffer := make([]byte, 4096)
		return &buffer
	},
}

type processIO struct {
	io runc.IO

	uri   *url.URL
	copy  bool
	stdio stdio.Stdio

	streams []io.ReadWriteCloser
}

func (p *processIO) Close() error {
	if p.io != nil {
		return p.io.Close()
	}
	for _, s := range p.streams {
		s.Close()
	}
	return nil
}

func (p *processIO) IO() runc.IO {
	return p.io
}

func (p *processIO) Copy(ctx context.Context, wg *sync.WaitGroup) (io.Closer, error) {
	if !p.copy {
		var c io.Closer
		if p.stdio.Stdin != "" {
			var err error
			c, err = fifo.OpenFifo(context.Background(), p.stdio.Stdin, unix.O_WRONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				return nil, fmt.Errorf("failed to open stdin fifo %s: %w", p.stdio.Stdin, err)
			}
		}
		return c, nil
	}
	var cwg sync.WaitGroup
	c, err := copyPipes(ctx, p.IO(), p.stdio.Stdin, p.stdio.Stdout, p.stdio.Stderr, p.streams, wg, &cwg)
	if err != nil {
		return nil, fmt.Errorf("unable to copy pipes: %w", err)
	}

	cwg.Wait()
	return c, nil
}

func createIO(ctx context.Context, id string, ioUID, ioGID int, stdio stdio.Stdio, ss stream.Manager) (*processIO, error) {
	pio := &processIO{
		stdio: stdio,
	}
	if stdio.IsNull() {
		i, err := runc.NewNullIO()
		if err != nil {
			return nil, err
		}
		pio.io = i
		return pio, nil
	}
	u, err := url.Parse(stdio.Stdout)
	if err != nil {
		return nil, fmt.Errorf("unable to parse stdout uri: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = "fifo"
	}
	pio.uri = u
	switch u.Scheme {
	case "stream":
		c, err := getStream(stdio.Stdout, ss)
		if err != nil {
			return nil, err
		}
		streams := []io.ReadWriteCloser{c}
		if stdio.Stdout != stdio.Stderr && stdio.Stderr != "" {
			c, err = getStream(stdio.Stderr, ss)
			if err != nil {
				return nil, err
			}
			streams = append(streams, c)
		}

		log.G(ctx).WithField("id", id).WithField("streams", streams).Debug("using stream IO")
		//pio.io, err = newStreamIO(ctx, streams)
		pio.streams = streams
		pio.copy = true
		pio.io, err = runc.NewPipeIO(ioUID, ioGID, withConditionalIO(stdio))
	case "fifo":
		pio.copy = true
		pio.io, err = runc.NewPipeIO(ioUID, ioGID, withConditionalIO(stdio))
	case "binary":
		pio.io, err = NewBinaryIO(ctx, id, u)
	case "file":
		filePath := u.Path
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return nil, err
		}
		var f *os.File
		f, err = os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		f.Close()
		pio.stdio.Stdout = filePath
		pio.stdio.Stderr = filePath
		pio.copy = true
		pio.io, err = runc.NewPipeIO(ioUID, ioGID, withConditionalIO(stdio))
	default:
		return nil, fmt.Errorf("unsupported STDIO scheme %s: %w", u.Scheme, errdefs.ErrNotImplemented)
	}
	if err != nil {
		return nil, err
	}
	return pio, nil
}

func copyPipes(ctx context.Context, rio runc.IO, stdin, stdout, stderr string, streams []io.ReadWriteCloser, wg, cwg *sync.WaitGroup) (io.Closer, error) {
	var sameFile *countingWriteCloser
	for _, i := range []struct {
		name  string
		index int
		dest  func(wc io.WriteCloser, rc io.Closer)
	}{
		{
			name:  stdout,
			index: 0,
			dest: func(wc io.WriteCloser, rc io.Closer) {
				wg.Add(1)
				cwg.Add(1)
				go func() {
					cwg.Done()
					p := bufPool.Get().(*[]byte)
					defer bufPool.Put(p)
					if _, err := io.CopyBuffer(wc, rio.Stdout(), *p); err != nil {
						log.G(ctx).Warn("error copying stdout")
					}
					wg.Done()
					wc.Close()
					if rc != nil {
						rc.Close()
					}
				}()
			},
		}, {
			name:  stderr,
			index: 1,
			dest: func(wc io.WriteCloser, rc io.Closer) {
				wg.Add(1)
				cwg.Add(1)
				go func() {
					cwg.Done()
					p := bufPool.Get().(*[]byte)
					defer bufPool.Put(p)
					if _, err := io.CopyBuffer(wc, rio.Stderr(), *p); err != nil {
						log.G(ctx).Warn("error copying stderr")
					}
					wg.Done()
					wc.Close()
					if rc != nil {
						rc.Close()
					}
				}()
			},
		},
	} {
		var (
			fw io.WriteCloser
			fr io.Closer
		)
		if streams != nil {
			idx := i.index
			if idx+1 >= len(streams) {
				idx = len(streams) - 1
			}
			fw = streams[idx]
		} else {
			ok, err := fifo.IsFifo(i.name)
			if err != nil {
				return nil, err
			}
			if ok {
				if fw, err = fifo.OpenFifo(ctx, i.name, syscall.O_WRONLY, 0); err != nil {
					return nil, fmt.Errorf("containerd-shim: opening w/o fifo %q failed: %w", i.name, err)
				}
				if fr, err = fifo.OpenFifo(ctx, i.name, syscall.O_RDONLY, 0); err != nil {
					return nil, fmt.Errorf("containerd-shim: opening r/o fifo %q failed: %w", i.name, err)
				}
			} else {
				if sameFile != nil {
					sameFile.bumpCount(1)
					i.dest(sameFile, nil)
					continue
				}
				if fw, err = os.OpenFile(i.name, syscall.O_WRONLY|syscall.O_APPEND, 0); err != nil {
					return nil, fmt.Errorf("containerd-shim: opening file %q failed: %w", i.name, err)
				}
				if stdout == stderr {
					sameFile = newCountingWriteCloser(fw, 1)
				}
			}
		}
		i.dest(fw, fr)
	}
	if stdin == "" {
		return nil, nil
	}
	var (
		c   io.Closer
		f   io.ReadCloser
		err error
	)
	if streams != nil {
		r, w := io.Pipe()
		go func() {
			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)
			if _, err := io.CopyBuffer(w, streams[0], *p); err != nil {
				log.G(ctx).WithError(err).Warn("error copying stdin")
			}
			w.Close()
		}()
		f = r
		c = w
	} else {
		f, err = fifo.OpenFifo(context.Background(), stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("containerd-shim: opening %s failed: %s", stdin, err)
		}
		c = f
	}
	cwg.Add(1)
	go func() {
		cwg.Done()
		p := bufPool.Get().(*[]byte)
		defer bufPool.Put(p)

		io.CopyBuffer(rio.Stdin(), f, *p)
		rio.Stdin().Close()
		f.Close()
	}()
	return c, nil
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

// newStreamIO connects stream to runc.IO
func newStreamIO(ctx context.Context, streams []io.ReadWriteCloser) (_ runc.IO, err error) {
	if len(streams) == 0 {
		return nil, errors.New("no streams provided")
	}
	return &streamIO{
		streams: streams,
	}, nil
}

type streamIO struct {
	streams []io.ReadWriteCloser
}

func (s *streamIO) Close() error {
	var result []error

	for _, rw := range s.streams {
		if err := rw.Close(); err != nil {
			result = append(result, err)
		}
	}

	return errors.Join(result...)
}

func (s *streamIO) Stdin() io.WriteCloser {
	return nil
}

func (s *streamIO) Stdout() io.ReadCloser {
	return nil
}

func (s *streamIO) Stderr() io.ReadCloser {
	return nil
}

func (s *streamIO) Set(cmd *exec.Cmd) {
	cmd.Stdin = s.streams[0]
	cmd.Stdout = s.streams[0]
	if len(s.streams) > 1 {
		cmd.Stderr = s.streams[1]
	} else {
		cmd.Stderr = s.streams[0]
	}
}

// NewBinaryIO runs a custom binary process for pluggable shim logging
func NewBinaryIO(ctx context.Context, id string, uri *url.URL) (_ runc.IO, err error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	var closers []func() error
	defer func() {
		if err == nil {
			return
		}
		result := []error{err}
		for _, fn := range closers {
			result = append(result, fn())
		}
		err = errors.Join(result...)
	}()

	out, err := newPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipes: %w", err)
	}
	closers = append(closers, out.Close)

	serr, err := newPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipes: %w", err)
	}
	closers = append(closers, serr.Close)

	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	closers = append(closers, r.Close, w.Close)

	cmd := NewBinaryCmd(uri, id, ns)
	cmd.ExtraFiles = append(cmd.ExtraFiles, out.r, serr.r, w)
	// don't need to register this with the reaper or wait when
	// running inside a shim
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start binary process: %w", err)
	}
	closers = append(closers, func() error { return cmd.Process.Kill() })

	// close our side of the pipe after start
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("failed to close write pipe after start: %w", err)
	}

	// wait for the logging binary to be ready
	b := make([]byte, 1)
	if _, err := r.Read(b); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read from logging binary: %w", err)
	}

	return &binaryIO{
		cmd: cmd,
		out: out,
		err: serr,
	}, nil
}

type binaryIO struct {
	cmd      *exec.Cmd
	out, err *pipe
}

func (b *binaryIO) CloseAfterStart() error {
	var result []error

	for _, v := range []*pipe{b.out, b.err} {
		if v != nil {
			if err := v.r.Close(); err != nil {
				result = append(result, err)
			}
		}
	}

	return errors.Join(result...)
}

func (b *binaryIO) Close() error {
	var result []error

	for _, v := range []*pipe{b.out, b.err} {
		if v != nil {
			if err := v.Close(); err != nil {
				result = append(result, err)
			}
		}
	}

	if err := b.cancel(); err != nil {
		result = append(result, err)
	}

	return errors.Join(result...)
}

func (b *binaryIO) cancel() error {
	if b.cmd == nil || b.cmd.Process == nil {
		return nil
	}

	// Send SIGTERM first, so logger process has a chance to flush and exit properly
	if err := b.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		result := []error{fmt.Errorf("failed to send SIGTERM: %w", err)}

		log.L.WithError(err).Warn("failed to send SIGTERM signal, killing logging shim")

		if err := b.cmd.Process.Kill(); err != nil {
			result = append(result, fmt.Errorf("failed to kill process after faulty SIGTERM: %w", err))
		}

		return errors.Join(result...)
	}

	done := make(chan error, 1)
	go func() {
		done <- b.cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(binaryIOProcTermTimeout):
		log.L.Warn("failed to wait for shim logger process to exit, killing")

		err := b.cmd.Process.Kill()
		if err != nil {
			return fmt.Errorf("failed to kill shim logger process: %w", err)
		}

		return nil
	}
}

func (b *binaryIO) Stdin() io.WriteCloser {
	return nil
}

func (b *binaryIO) Stdout() io.ReadCloser {
	return nil
}

func (b *binaryIO) Stderr() io.ReadCloser {
	return nil
}

func (b *binaryIO) Set(cmd *exec.Cmd) {
	if b.out != nil {
		cmd.Stdout = b.out.w
	}
	if b.err != nil {
		cmd.Stderr = b.err.w
	}
}

func newPipe() (*pipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &pipe{
		r: r,
		w: w,
	}, nil
}

type pipe struct {
	r *os.File
	w *os.File
}

func (p *pipe) Close() error {
	var result []error

	if err := p.w.Close(); err != nil {
		result = append(result, fmt.Errorf("pipe: failed to close write pipe: %w", err))
	}

	if err := p.r.Close(); err != nil {
		result = append(result, fmt.Errorf("pipe: failed to close read pipe: %w", err))
	}

	return errors.Join(result...)
}
