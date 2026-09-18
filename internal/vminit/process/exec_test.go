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

package process

import (
	"io"
	"testing"

	"github.com/containerd/console"
	"github.com/stretchr/testify/require"
)

type trackedCloser struct{ closed bool }

func (c *trackedCloser) Close() error { c.closed = true; return nil }

type trackedConsole struct {
	console.Console
	trackedCloser
}

func (c *trackedConsole) Close() error { return c.trackedCloser.Close() }

func TestDeleteTTYClosesConsoleAndStreams(t *testing.T) {
	stdin := &trackedCloser{}
	cons := &trackedConsole{}
	e := &execProcess{id: "tty", path: t.TempDir(), console: cons, closers: []io.Closer{stdin}}
	require.NoError(t, e.delete(t.Context()))
	require.True(t, stdin.closed, "TTY stdin remains open without processIO")
	require.True(t, cons.closed, "TTY console remains open without processIO")
}
