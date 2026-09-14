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

package transfer

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	ctransfer "github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/errdefs"
)

const mediaTypeTar = "application/x-tar"

// containerRoot is the root directory of the container's filesystem from
// inside its mount namespace, which is where ContainerFS.Do runs the archive
// operations below.
const containerRoot = "/"

// ContainerFS runs an operation with a container's filesystem as the root
// directory.
//
// The operation must see the container's own view, with its mounts applied.
// The bundle's rootfs directory is not that view: the runtime applies the
// container's mounts inside the container's mount namespace, so in the bundle
// rootfs every mount destination is an empty directory shadowing the content
// the container actually sees. Copying through those shadowed entries writes
// files the container will never observe and reads content the container does
// not have.
//
// Implementations confine the operation to the container's filesystem, so
// paths cannot reach anything outside it.
type ContainerFS interface {
	Do(containerID string, fn func() error) error
}

// NewContainerFSTransferrer returns a Transferrer that handles
// ContainerPath + ReadStream/WriteStream transfer pairs, running archive
// operations inside the container's filesystem via ctrFS.
func NewContainerFSTransferrer(ctrFS ContainerFS) ctransfer.Transferrer {
	return &containerFSTransferrer{ctrFS: ctrFS}
}

type containerFSTransferrer struct {
	ctrFS ContainerFS
}

func (t *containerFSTransferrer) Transfer(ctx context.Context, src, dst any, opts ...ctransfer.Opt) error {
	switch s := src.(type) {
	case *ContainerPath:
		// Copy-from: ContainerPath -> WriteStream
		d, ok := dst.(*WriteStream)
		if !ok {
			return errdefs.ErrNotImplemented
		}
		// Set the stream up inside the closure so that a container whose
		// filesystem cannot be reached is reported without a stream having
		// been created for it. Stream setup performs no path resolution, so
		// running it here does not depend on which filesystem is in view.
		err := t.ctrFS.Do(s.ContainerID, func() error {
			w := d.Writer(ctx)
			defer w.Close()
			return writePath(containerRoot, s.Path, w, d.MediaType, s.NoWalk)
		})
		if err != nil {
			// The client's stream was established before this transferrer
			// was reached. If the container's filesystem could not be
			// resolved the closure never ran, so nothing has released the
			// stream and the client would wait on it indefinitely.
			d.Close()
		}
		return err

	case *ReadStream:
		// Copy-to: ReadStream -> ContainerPath
		d, ok := dst.(*ContainerPath)
		if !ok {
			return errdefs.ErrNotImplemented
		}
		err := t.ctrFS.Do(d.ContainerID, func() error {
			return readPath(s.Reader(ctx), containerRoot, d.Path, s.MediaType, d.PreserveOwnership)
		})
		if err != nil {
			// As above: the closure may never have run, leaving the client
			// sending on a stream nothing will read.
			s.Close()
		}
		return err
	}

	return errdefs.ErrNotImplemented
}

// rootRel converts a path expressed in the container's view, which may be
// absolute or contain parent-directory components, into a path relative to the
// filesystem root. Cleaning is lexical: "../" sequences collapse before the
// path is resolved rather than as the container would resolve them, so the
// result never names anything above the root. An empty result becomes ".",
// the root itself.
func rootRel(p string) string {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	if p == "" {
		return "."
	}
	return p
}

// rootJoin resolves a path relative to the filesystem root into a path to
// operate on.
//
// root is "/" in production, where the container's filesystem is the thread's
// root directory. Tests pass a directory instead so that the archive logic can
// be exercised without privileges. That substitution is textual only: a
// directory used as root is not a boundary, and symlinks resolved beneath it
// follow the real filesystem root. How paths actually resolve is therefore not
// something the tests here can establish; that is covered end to end, against a
// real container, by the shim conformance suite in test/shim.
func rootJoin(root, rel string) string {
	if rel == "." {
		return root
	}
	return filepath.Join(root, rel)
}

// writePath creates a tar archive from the given path within root and writes
// it to w. When noWalk is true and path is a directory, only the directory
// entry itself is included without walking into it.
//
// Symlinks encountered along the path are followed as the container follows
// them, including absolute ones, because in production root is the container's
// own root directory and the kernel resolves against it.
func writePath(root, src string, w io.Writer, mediaType string, noWalk bool) error {
	if mediaType != mediaTypeTar {
		return fmt.Errorf("unsupported media type %q: %w", mediaType, errdefs.ErrNotImplemented)
	}

	relPath := rootRel(src)
	full := rootJoin(root, relPath)

	fi, err := os.Lstat(full)
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", src, err)
	}
	// Naming one of these directly is reported rather than skipped, so the
	// caller is told why nothing was archived instead of receiving an empty
	// or truncated result. A symlink is exempt: it is recorded by its target
	// string and never followed, so a link that happens to point at one of
	// these filesystems reads nothing from it.
	if fs := pseudoFS(full); fi.Mode()&os.ModeSymlink == 0 && fs != "" {
		return fmt.Errorf("cannot archive %s from the %s filesystem: %w", src, fs, errdefs.ErrInvalidArgument)
	}

	// The top-level entry name is the basename of the requested
	// path. When the caller asks for the whole filesystem (path "/"),
	// relPath is "." and baseName is "."; child entries then drop
	// the leading "./" via path.Join, so the tar contains
	// "bin/sh" rather than leaking the name of the directory root
	// happens to be.
	baseName := path.Base(relPath)

	tw := tar.NewWriter(w)

	if !fi.IsDir() || noWalk {
		if err := writeTarEntry(root, tw, relPath, fi, baseName); err != nil {
			tw.Close()
			return err
		}
		return tw.Close()
	}

	if err := fs.WalkDir(os.DirFS(root), relPath, func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		// Skip kernel-generated filesystems mounted below the requested
		// path. A recursive copy that happened to span /proc or /sys would
		// otherwise fail on the first of their files, taking the rest of
		// the archive with it. Checking directories is enough: their
		// entries share the filesystem unless one is itself a mount point,
		// and a file that is one is either a regular file that archives
		// normally or a device node, which is recorded without its
		// contents either way.
		if d.IsDir() {
			if fsName := pseudoFS(rootJoin(root, walkPath)); fsName != "" {
				return fs.SkipDir
			}
		}

		// walkPath is always slash-separated (fs.FS contract) and
		// rooted at relPath. Derive the entry's path within the walk
		// without using strings.TrimPrefix: when relPath is "." a
		// naive TrimPrefix would strip the leading "." from dotfiles
		// like ".bashrc" (since "." is a string prefix of ".bashrc").
		var rel string
		switch {
		case walkPath == relPath:
			// The root entry itself.
			rel = ""
		case relPath == ".":
			// Walking from the root itself: walkPath is already the
			// entry name relative to the root.
			rel = walkPath
		default:
			// Walking a subdirectory: strip "relPath/" prefix.
			rel = strings.TrimPrefix(walkPath, relPath+"/")
		}
		var name string
		if rel == "" {
			name = baseName
		} else {
			name = path.Join(baseName, rel)
		}

		return writeTarEntry(root, tw, walkPath, info, name)
	}); err != nil {
		tw.Close()
		return err
	}
	return tw.Close()
}

// writeTarEntry writes a single tar entry. srcPath is interpreted relative to
// root.
func writeTarEntry(root string, tw *tar.Writer, srcPath string, fi os.FileInfo, name string) error {
	full := rootJoin(root, srcPath)

	header, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	header.Name = name

	if fi.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(full)
		if err != nil {
			return err
		}
		header.Linkname = link
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	if fi.Mode().IsRegular() {
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
	}

	return nil
}

// readPath reads a tar archive from r and extracts it under path within root.
// When preserveOwnership is true, extracted files have their UID/GID set from
// the tar headers.
//
// Entry names are cleaned relative to the destination, so "../" in a name
// cannot walk out of it. Symlinks are a different matter: a symlink already
// present at the destination, or created by an earlier entry in the same
// archive, redirects later entries wherever it points, exactly as it would for
// a process in the container writing the same paths. Extraction is bounded by
// the container's filesystem rather than by the destination directory, because
// in production root is the container's root directory and the kernel does not
// resolve above a thread's root.
func readPath(r io.Reader, root, dstPath, mediaType string, preserveOwnership bool) error {
	if mediaType != mediaTypeTar {
		return fmt.Errorf("unsupported media type %q: %w", mediaType, errdefs.ErrNotImplemented)
	}

	dst := rootJoin(root, rootRel(dstPath))
	if err := os.MkdirAll(dst, 0755); err != nil {
		return fmt.Errorf("failed to create destination: %w", err)
	}
	// Extracting here would write into kernel interfaces rather than
	// create files.
	if fsName := pseudoFS(dst); fsName != "" {
		return fmt.Errorf("cannot extract to %s on the %s filesystem: %w", dstPath, fsName, errdefs.ErrInvalidArgument)
	}

	// Kernel-generated filesystems mounted below the destination are skipped
	// the same way they are when archiving. Naming one as the destination is
	// refused above, where it is what the caller asked for; meeting one part
	// way through an archive is incidental and should not cost the rest of
	// the copy.
	//
	// The answer is kept per directory rather than per entry: an archive
	// holds many entries per directory, and what is mounted under a path
	// does not change while it is being extracted into.
	holders := map[string]string{}
	pseudoHolder := func(dir string) string {
		holder, ok := holders[dir]
		if !ok {
			holder = pseudoFSHolder(dir)
			holders[dir] = holder
		}
		return holder
	}

	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}

		// Clean the entry name relative to "/" so any "../" sequences
		// collapse before the name is joined onto the destination.
		entryName := strings.TrimPrefix(path.Clean("/"+header.Name), "/")
		if entryName == "" {
			// Names that resolve to the destination itself (e.g. "."
			// or "/") have nothing to extract.
			continue
		}

		if pseudoHolder(filepath.Dir(filepath.Join(dst, entryName))) != "" {
			continue
		}

		if err := extractTarEntry(dst, entryName, header, tr, preserveOwnership); err != nil {
			return err
		}
	}
}

// extractTarEntry writes a single archive entry. dst is the directory the
// archive is being extracted into and target names the entry within it.
func extractTarEntry(dst, target string, header *tar.Header, r io.Reader, preserveOwnership bool) error {
	switch header.Typeflag {
	case tar.TypeDir, tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
	default:
		// Device nodes, pipes and sockets are not created from an archive:
		// these operations carry more authority than the container itself,
		// and an archive naming a device is asking for one to be made with
		// it. Nothing is removed for them either, so a container's device
		// tree survives an archive that happens to describe it.
		return nil
	}

	full := filepath.Join(dst, target)

	// Make room for the entry at the last path component. Two directories
	// merge rather than one replacing the other. Anything else gives way,
	// which is also what keeps a device node or pipe from being opened and
	// written into as though it were a file.
	if fi, err := os.Lstat(full); err == nil {
		switch {
		case fi.IsDir() && header.Typeflag == tar.TypeDir:
		case fi.Mode()&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeIrregular) != 0:
			// Removing one of these would leave the container without an
			// interface it expects, so the collision is reported instead.
			return fmt.Errorf("refusing to replace the special file at %s: %w", target, errdefs.ErrFailedPrecondition)
		default:
			// Remove, not RemoveAll: a populated directory standing where a
			// file belongs is a conflict to report, not a tree to delete.
			if err := os.Remove(full); err != nil {
				return fmt.Errorf("failed to replace %s: %w", target, err)
			}
		}
	}

	switch header.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(full, os.FileMode(header.Mode)); err != nil {
			return err
		}
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return err
		}
		// The target string is stored verbatim, so the link means the same
		// thing to the container as it did in the archive it came from.
		if err := os.Symlink(header.Linkname, full); err != nil {
			return err
		}
	case tar.TypeLink:
		// Hardlink source names another entry in the same archive, so clean
		// it the same way as the entry name and resolve it against the same
		// destination.
		linkSrc := strings.TrimPrefix(path.Clean("/"+header.Linkname), "/")
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return err
		}
		if err := os.Link(filepath.Join(dst, linkSrc), full); err != nil {
			return err
		}
	}

	if preserveOwnership {
		if err := os.Lchown(full, header.Uid, header.Gid); err != nil {
			return fmt.Errorf("failed to chown %s: %w", target, err)
		}
	}

	return nil
}
