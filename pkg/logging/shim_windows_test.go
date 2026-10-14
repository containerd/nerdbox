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

package logging

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

// TestServeShimLogReconnect simulates containerd crashing and restarting
// while the shim keeps running: the shim must keep accepting stderr writes
// after the first reader goes away, and the restarted containerd must be able
// to reconnect and receive new log lines.
func TestServeShimLogReconnect(t *testing.T) {
	pipeName := fmt.Sprintf(`\\.\pipe\nerdbox-test-shim-log-%d`, time.Now().UnixNano())
	l, err := winio.ListenPipe(pipeName, nil)
	if err != nil {
		t.Fatal(err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	defer pw.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveShimLog(l, pr, io.Discard)
	}()
	defer func() {
		pw.Close()
		<-done
	}()

	first := dialShimLog(t, pipeName)
	waitForLine(t, pw, first, "first")
	first.Close()

	// With nothing draining the pipe, this blocks once the pipe buffer is
	// full, as would every later log write in the shim.
	written := make(chan error, 1)
	go func() {
		_, err := pw.Write(bytes.Repeat([]byte("x"), 1<<20))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stderr write blocked after the log reader disconnected")
	}

	second := dialShimLog(t, pipeName)
	defer second.Close()
	waitForLine(t, pw, second, "second")
}

func dialShimLog(t *testing.T, pipeName string) net.Conn {
	t.Helper()

	timeout := 5 * time.Second
	c, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		t.Fatalf("reconnecting to shim log: %v", err)
	}
	return c
}

// waitForLine writes line to w until it is read back from c. The connection
// is registered by serveShimLog's accept goroutine after DialPipe returns, so
// a single write could race with it and be dropped.
func waitForLine(t *testing.T, w io.Writer, c net.Conn, line string) {
	t.Helper()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fmt.Fprintln(w, line)
			}
		}
	}()

	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	s := bufio.NewScanner(c)
	s.Buffer(make([]byte, 0, 64*1024), 2<<20)
	for s.Scan() {
		if strings.HasSuffix(s.Text(), line) {
			return
		}
	}
	t.Fatalf("did not receive %q: %v", line, s.Err())
}

// TestServeShimLogStalledReader checks that a log reader which stays
// connected but stops reading doesn't stop a reconnecting containerd from
// taking over the log.
func TestServeShimLogStalledReader(t *testing.T) {
	l := &chanListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveShimLog(l, pr, io.Discard)
	}()
	defer func() {
		pw.Close()
		<-done
	}()

	stalled := newStalledConn()
	defer stalled.Close()
	l.conns <- stalled
	go fmt.Fprintln(pw, "first") //nolint:errcheck
	select {
	case <-stalled.writing:
	case <-time.After(10 * time.Second):
		t.Fatal("log was not written to the first connection")
	}

	// The drain is now blocked writing to the stalled connection. A new
	// connection must replace it and receive the next line.
	server, client := net.Pipe()
	defer client.Close()
	select {
	case l.conns <- server:
	case <-time.After(10 * time.Second):
		t.Fatal("reconnecting log reader was not accepted")
	}
	waitForLine(t, pw, client, "second")
}

// chanListener is a net.Listener that accepts the connections sent on conns.
type chanListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return nil }

// stalledConn is a log reader that is connected but never reads: Write
// blocks until the connection is closed.
type stalledConn struct {
	net.Conn

	writing   chan struct{}
	closed    chan struct{}
	writeOnce sync.Once
	closeOnce sync.Once
}

func newStalledConn() *stalledConn {
	return &stalledConn{writing: make(chan struct{}), closed: make(chan struct{})}
}

func (c *stalledConn) Write([]byte) (int, error) {
	c.writeOnce.Do(func() { close(c.writing) })
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stalledConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
