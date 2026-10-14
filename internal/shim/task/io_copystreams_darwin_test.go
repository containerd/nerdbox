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
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Tests that only make sense on macOS, where the kernel and the Go runtime
// treat FIFOs differently from Linux. The behavioural tests in
// io_copystreams_polled_test.go and io_fifo_poller_test.go also run here, against
// the real kqueue poller.

func init() {
	// On macOS the implementation the shim really runs has to pass the
	// teardown suite as well; reverting startCopy to the blocking
	// implementation makes those tests fail.
	teardownImpls = append(teardownImpls, copyImpl{"platform", startCopy})
}

// TestStartCopyReleasesFifos pins what the shim runs on macOS: the polled
// implementation, which has an abort for ioShutdown. The blocking
// implementation has none, and it is the one that leaves a goroutine and an OS
// thread parked in read(2) that nothing can release.
func TestStartCopyReleasesFifos(t *testing.T) {
	stdoutPath := mkfifo(t, "stdout")
	openClientEnd(t, stdoutPath, syscall.O_RDONLY)
	out := newFakeConn()
	_, _, abort, err := startCopy(t.Context(), [3]io.ReadWriteCloser{nil, out, nil}, "", stdoutPath, "", make(chan struct{}))
	require.NoError(t, err)
	require.NotNil(t, abort, "startCopy on darwin must be the implementation that can release its FIFOs")
	abort()

	// abort releases the FIFO descriptors only. The output copy is also
	// parked reading the guest's stream, which ioShutdown ends by closing the
	// transport; do the same here so the goroutine does not outlive the test,
	// where the tests that count copy goroutines would find it.
	out.Close()
	requireNoCopyGoroutines(t, "a copy goroutine is still running after abort and closing the stream")
}

// TestOsOpenFileKeepsFifosOffThePoller documents why the polled implementation
// exists. If Go starts registering FIFOs opened by os.OpenFile with kqueue on
// darwin (golang/go#24164), a read deadline works on them and this logs it:
// the workaround in io_fifo_poller.go may then be removable, provided macOS
// also delivers the last-writer-close wakeup reliably (which the stress test
// below probes). It does not fail, since nothing is wrong when that happens.
func TestOsOpenFileKeepsFifosOffThePoller(t *testing.T) {
	path := mkfifo(t, "fifo")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	if err := f.SetReadDeadline(time.Now().Add(time.Hour)); err == nil {
		t.Log("os.OpenFile now registers FIFOs with the poller on darwin: the polled FIFO implementation may no longer be needed")
	} else {
		require.ErrorIs(t, err, os.ErrNoDeadline)
	}
}

// TestStdinEOFStressLong is the kqueue counterpart of the lost last-writer-close
// reproducer: tens of thousands of stdin copies whose only way to finish is to
// notice that the last writer went away. A copy that waits for the kernel to
// announce it hangs a measurable fraction of the time on macOS; this one polls
// and never does.
func TestStdinEOFStressLong(t *testing.T) {
	if testing.Short() {
		t.Skip("long stress test")
	}
	stressStdinEOF(t, copyImpl{"platform", startCopy}, 8, 2500)
}
