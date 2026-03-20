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

package libkrun

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/containerd/log"
)

func setupConsole(_ context.Context, vmc *vmcontext, _ string) (io.ReadCloser, error) {
	pipeName := fmt.Sprintf(`\\.\pipe\krun-console-%d`, os.Getpid())
	if err := vmc.SetConsole(pipeName); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go connectAndCopyConsole(pipeName, pw)
	return pr, nil
}

func connectAndCopyConsole(pipeName string, pw *io.PipeWriter) {
	defer pw.Close()
	var conn io.ReadCloser
	timeout := 5 * time.Second
	for d := 10 * time.Millisecond; d < timeout; d *= 2 {
		c, err := winio.DialPipe(pipeName, &timeout)
		if err == nil {
			conn = c
			break
		}
		log.L.WithError(err).Debugf("console pipe not ready, retrying in %s...", d)
		time.Sleep(d)
	}
	if conn == nil {
		log.L.Warnf("failed to connect to console pipe %s within timeout", pipeName)
		return
	}
	defer conn.Close()
	log.L.Debugf("connected to console pipe %s", pipeName)
	if _, err := io.Copy(pw, conn); err != nil {
		log.L.WithError(err).Debug("console pipe copy ended")
	}
}
