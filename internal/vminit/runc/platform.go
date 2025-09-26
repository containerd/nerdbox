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

package runc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/fifo"

	"github.com/containerd/nerdbox/internal/vminit/process"
	"github.com/containerd/nerdbox/internal/vminit/stream"
)

var bufPool = sync.Pool{
	New: func() interface{} {
		// setting to 4096 to align with PIPE_BUF
		// http://man7.org/linux/man-pages/man7/pipe.7.html
		buffer := make([]byte, 4096)
		return &buffer
	},
}

// NewPlatform returns a linux platform for use with I/O operations
func NewPlatform(m stream.Manager) (stdio.Platform, error) {
	epoller, err := console.NewEpoller()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize epoller: %w", err)
	}
	go epoller.Wait()
	return &linuxPlatform{
		epoller: epoller,
		streams: m,
	}, nil
}

type linuxPlatform struct {
	epoller *console.Epoller
	streams stream.Manager
}

func (p *linuxPlatform) CopyConsole(ctx context.Context, console console.Console, id, stdin, stdout, stderr string, wg *sync.WaitGroup) (cons console.Console, retErr error) {
	if p.epoller == nil {
		return nil, errors.New("uninitialized epoller")
	}

	epollConsole, err := p.epoller.Add(console)
	if err != nil {
		return nil, err
	}
	var cstdin io.Closer

	var cwg sync.WaitGroup
	if stdin != "" {
		var in io.ReadCloser
		if s, ok := strings.CutPrefix(stdin, "stream://"); ok {
			var sid uint64
			sid, err = strconv.ParseUint(s, 10, 32)
			if err != nil {
				return nil, err
			}
			in, err = p.streams.Get(uint32(sid))
			cstdin = in
		} else {
			in, err = fifo.OpenFifo(ctx, stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
			cstdin = in
		}
		if err != nil {
			return nil, err
		}
		cwg.Add(1)
		go func() {
			cwg.Done()
			bp := bufPool.Get().(*[]byte)
			defer bufPool.Put(bp)
			io.CopyBuffer(epollConsole, in, *bp)
			// we need to shutdown epollConsole when pipe broken
			epollConsole.Shutdown(p.epoller.CloseConsole)
			epollConsole.Close()
		}()
	}

	uri, err := url.Parse(stdout)
	if err != nil {
		return nil, fmt.Errorf("unable to parse stdout uri: %w", err)
	}

	switch uri.Scheme {
	case "stream":
		sid, err := strconv.ParseUint(strings.TrimPrefix(stdout, "stream://"), 10, 32)
		if err != nil {
			return nil, err
		}
		out, err := p.streams.Get(uint32(sid))
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		cwg.Add(1)
		go func() {
			cwg.Done()
			buf := bufPool.Get().(*[]byte)
			defer bufPool.Put(buf)
			io.CopyBuffer(out, epollConsole, *buf)

			out.Close()
			wg.Done()
		}()
		cwg.Wait()
	case "binary":
		ns, err := namespaces.NamespaceRequired(ctx)
		if err != nil {
			return nil, err
		}

		cmd := process.NewBinaryCmd(uri, id, ns)

		// In case of unexpected errors during logging binary start, close open pipes
		var filesToClose []*os.File

		defer func() {
			if retErr != nil {
				process.CloseFiles(filesToClose...)
			}
		}()

		// Create pipe to be used by logging binary for Stdout
		outR, outW, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("failed to create stdout pipes: %w", err)
		}
		filesToClose = append(filesToClose, outR)

		// Stderr is created for logging binary but unused when terminal is true
		serrR, _, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("failed to create stderr pipes: %w", err)
		}
		filesToClose = append(filesToClose, serrR)

		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		filesToClose = append(filesToClose, r)

		cmd.ExtraFiles = append(cmd.ExtraFiles, outR, serrR, w)

		wg.Add(1)
		cwg.Add(1)
		go func() {
			cwg.Done()
			io.Copy(outW, epollConsole)
			outW.Close()
			wg.Done()
		}()

		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("failed to start logging binary process: %w", err)
		}

		// Close our side of the pipe after start
		if err := w.Close(); err != nil {
			return nil, fmt.Errorf("failed to close write pipe after start: %w", err)
		}

		// Wait for the logging binary to be ready
		b := make([]byte, 1)
		if _, err := r.Read(b); err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read from logging binary: %w", err)
		}
		cwg.Wait()

	default:
		outw, err := fifo.OpenFifo(ctx, stdout, syscall.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		outr, err := fifo.OpenFifo(ctx, stdout, syscall.O_RDONLY, 0)
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		cwg.Add(1)
		go func() {
			cwg.Done()
			buf := bufPool.Get().(*[]byte)
			defer bufPool.Put(buf)
			io.CopyBuffer(outw, epollConsole, *buf)

			outw.Close()
			outr.Close()
			wg.Done()
		}()
		cwg.Wait()
	}
	if cstdin != nil {
		cons = &closingConsole{
			Console:    epollConsole,
			closeStdin: cstdin,
		}
	} else {
		cons = epollConsole
	}

	return
}

func (p *linuxPlatform) ShutdownConsole(ctx context.Context, cons console.Console) error {
	if p.epoller == nil {
		return errors.New("uninitialized epoller")
	}
	epollConsole, ok := cons.(*console.EpollConsole)
	if !ok {
		return fmt.Errorf("expected EpollConsole, got %#v", cons)
	}
	return epollConsole.Shutdown(p.epoller.CloseConsole)
}

func (p *linuxPlatform) Close() error {
	return p.epoller.Close()
}

type closingConsole struct {
	console.Console

	closeStdin io.Closer
}

func (c *closingConsole) StdinCloser() io.Closer {
	return c.closeStdin
}
