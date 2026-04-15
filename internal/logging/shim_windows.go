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
	"fmt"
	"io"
	"os"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// openShimLog creates a named pipe for containerd to read shim logs and
// redirects os.Stderr to it, mirroring the containerd shim framework's
// Windows openLog behavior.
//
// The namespace and id are read from os.Args by the caller
// ([SetupShimLog]) and passed through the package-level shimNamespace
// and shimID variables.
func openShimLog(ns, id string) io.Writer {
	if ns == "" || id == "" {
		return os.Stderr
	}
	pipeName := fmt.Sprintf(`\\.\pipe\containerd-shim-%s-%s-log`, ns, id)
	l, err := winio.ListenPipe(pipeName, nil)
	if err != nil {
		return os.Stderr
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		l.Close()
		return os.Stderr
	}

	oldStderr := os.Stderr
	os.Stderr = pw
	windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(pw.Fd())) //nolint:errcheck

	go func() {
		conn, err := l.Accept()
		l.Close()

		var w io.Writer = oldStderr
		if err == nil {
			w = io.MultiWriter(oldStderr, conn)
			defer conn.Close()
		}
		io.Copy(w, pr) //nolint:errcheck
	}()

	return pw
}
