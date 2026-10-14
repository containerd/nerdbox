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
