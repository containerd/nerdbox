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

package logging

import (
	"context"
	"io"
	"os"

	"github.com/containerd/fifo"
	"golang.org/x/sys/unix"
)

// openShimLog opens the "log" FIFO created by containerd and dup2's it
// onto stderr so that any raw stderr writes also reach containerd.
func openShimLog(string, string) io.Writer {
	f, err := fifo.OpenFifoDup2(context.Background(), "log", unix.O_WRONLY, 0700, int(os.Stderr.Fd()))
	if err != nil {
		return os.Stderr
	}
	return f
}
