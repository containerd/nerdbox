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

package socketforward

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	socketforward "github.com/containerd/nerdbox/api/services/socketforward/v1"
)

func TestBind(t *testing.T) {
	dir, err := os.MkdirTemp("", "sf-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "agent.sock")
	s := NewService(nil)
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{{
		ForwardID: "test", SocketPath: path, Mode: 0600,
		Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()),
	}}})
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.Equal(t, uint32(os.Getuid()), info.Sys().(*syscall.Stat_t).Uid)
	require.Equal(t, uint32(os.Getgid()), info.Sys().(*syscall.Stat_t).Gid)
	require.NoError(t, assertNoStagingDirectories(dir))
}

func TestBindRejectsReservedOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  uint32
		gid  uint32
	}{
		{name: "UID", uid: ^uint32(0), gid: 1000},
		{name: "GID", uid: 1000, gid: ^uint32(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			firstPath := filepath.Join(dir, "first.sock")
			reservedPath := filepath.Join(dir, "reserved.sock")
			s := NewService(nil)
			t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
			_, err := s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{
				{ForwardID: "first", SocketPath: firstPath},
				{ForwardID: "reserved", SocketPath: reservedPath, Mode: 0600, Uid: tc.uid, Gid: tc.gid},
			}})
			require.ErrorContains(t, err, "reserved guest socket UID or GID")
			require.Empty(t, s.listeners)
			for _, path := range []string{firstPath, reservedPath} {
				_, err := os.Stat(path)
				require.True(t, os.IsNotExist(err), "failed bind left a socket at %s: %v", path, err)
			}
			require.NoError(t, assertNoStagingDirectories(dir))
		})
	}
}

func TestBindRejectsUnsupportedModeBeforeCreatingSockets(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.sock")
	s := NewService(nil)
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	_, err := s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{
		{ForwardID: "first", SocketPath: firstPath},
		{ForwardID: "unsupported", SocketPath: filepath.Join(dir, "other.sock"), Mode: 0666},
	}})
	require.ErrorContains(t, err, "unsupported guest socket mode")
	require.Empty(t, s.listeners)
	_, err = os.Lstat(firstPath)
	require.True(t, os.IsNotExist(err))
}

func assertNoStagingDirectories(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".socketfwd-") {
			return fmt.Errorf("private socket staging directory left behind: %s", entry.Name())
		}
	}
	return nil
}

func TestBindRollsBackPartialBind(t *testing.T) {
	dir, err := os.MkdirTemp("", "sf-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "agent.sock")
	s := NewService(nil)
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{
		{ForwardID: "first", SocketPath: path, Mode: 0600, Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())},
		{ForwardID: "second", SocketPath: "/dev/null/impossible.sock"},
	}})
	require.Error(t, err)
	require.Empty(t, s.listeners)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "partially bound socket remained accessible: %v", err)
	require.NoError(t, assertNoStagingDirectories(dir))
}

func TestShutdownRemovesForwardedSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "sf-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "agent.sock")
	s := NewService(nil)
	_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{{
		ForwardID: "test", SocketPath: path, Mode: 0600,
		Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()),
	}}})
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, s.Shutdown(t.Context()))
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "closed listener remains visible: %v", err)
	require.NoError(t, assertNoStagingDirectories(dir))
}

func TestBindWithoutPermissionsKeepsDefaultMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sock")
	// The legacy behavior is net.Listen's mode (subject to the process umask),
	// not the private 0600 mode requested by an owned socket.
	baseline, err := net.Listen("unix", filepath.Join(dir, "baseline.sock"))
	require.NoError(t, err)
	baselineInfo, err := os.Stat(filepath.Join(dir, "baseline.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, baseline.Close()) })
	s := NewService(nil)
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{{
		ForwardID: "test", SocketPath: path,
	}}})
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, baselineInfo.Mode().Perm(), info.Mode().Perm())
}

func TestBindRejectsDuplicateWithoutUnlinkingLiveListener(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sock")
	s := NewService(nil)
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	sock := &socketforward.Socket{ForwardID: "test", SocketPath: path, Mode: 0600,
		Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	_, err := s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{sock}})
	require.NoError(t, err)

	// A later bind must not remove the first listener's filesystem entry.
	_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{sock}})
	require.ErrorContains(t, err, "already bound")
	require.Len(t, s.listeners, 1)
	conn, err := net.Dial("unix", path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	aliasDir := filepath.Join(dir, "alias")
	require.NoError(t, os.Symlink(dir, aliasDir))
	for _, alias := range []struct {
		path    string
		wantErr string
	}{
		{path: dir + "/./agent.sock", wantErr: "non-canonical"},
		{path: aliasDir + "/../agent.sock", wantErr: "non-canonical"},
		{path: filepath.Join(aliasDir, "agent.sock"), wantErr: "already bound"},
	} {
		_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{
			{ForwardID: "new", SocketPath: filepath.Join(dir, "new.sock")},
			{ForwardID: "alias", SocketPath: alias.path, Mode: 0600,
				Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())},
		}})
		require.ErrorContains(t, err, alias.wantErr)
		require.Len(t, s.listeners, 1)
		_, err = os.Stat(filepath.Join(dir, "new.sock"))
		require.True(t, os.IsNotExist(err))
		conn, err = net.Dial("unix", path)
		require.NoError(t, err, "aliased bind disrupted original listener")
		require.NoError(t, conn.Close())
	}

	// The same applies when the duplicate is later in a single batch: no
	// earlier entry in that batch should be bound or left on disk.
	newPath := filepath.Join(dir, "new.sock")
	_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{
		{ForwardID: "new", SocketPath: newPath},
		{ForwardID: "duplicate", SocketPath: newPath},
	}})
	require.ErrorContains(t, err, "already bound")
	require.Len(t, s.listeners, 1)
	_, err = os.Stat(newPath)
	require.True(t, os.IsNotExist(err))
	conn, err = net.Dial("unix", path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestBindRollbackPreservesPreviouslyBoundListener(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.sock")
	newPath := filepath.Join(dir, "new.sock")
	s := NewService(nil)
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	_, err := s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{{
		ForwardID: "existing", SocketPath: path, Mode: 0600,
		Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()),
	}}})
	require.NoError(t, err)
	_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{
		{ForwardID: "new", SocketPath: newPath},
		{ForwardID: "invalid", SocketPath: "/dev/null/impossible.sock"},
	}})
	require.Error(t, err)
	require.Len(t, s.listeners, 1)
	_, err = os.Stat(newPath)
	require.True(t, os.IsNotExist(err))
	conn, err := net.Dial("unix", path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, assertNoStagingDirectories(dir))
}

func TestPrivateBindFailureRemovesStagingDirectory(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory cannot be replaced by a socket. Publication
	// must fail without leaking the protected staging directory or listener.
	path := filepath.Join(dir, "occupied")
	require.NoError(t, os.Mkdir(path, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "keep"), []byte("data"), 0600))
	s := NewService(nil)
	_, err := s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{{
		ForwardID: "test", SocketPath: path, Mode: 0600,
		Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()),
	}}})
	require.Error(t, err)
	require.Empty(t, s.listeners)
	require.NoError(t, assertNoStagingDirectories(dir))
	content, err := os.ReadFile(filepath.Join(path, "keep"))
	require.NoError(t, err)
	require.Equal(t, "data", string(content))
}

func TestPrivateBindReplacesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sock")
	stale, err := net.Listen("unix", path)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	staleInfo, err := os.Stat(path)
	require.NoError(t, err)

	s := NewService(nil)
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	_, err = s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{{
		ForwardID: "replacement", SocketPath: path, Mode: 0600,
		Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()),
	}}})
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(staleInfo, info))
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	conn, err := net.Dial("unix", path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, assertNoStagingDirectories(dir))
}

func TestShutdownDoesNotRemoveReplacementAtPrivatePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sock")
	s := NewService(nil)
	_, err := s.Bind(t.Context(), &socketforward.BindRequest{Sockets: []*socketforward.Socket{{
		ForwardID: "test", SocketPath: path, Mode: 0600,
		Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()),
	}}})
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0600))
	require.NoError(t, s.Shutdown(t.Context()))
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "replacement", string(content))
}
