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
	"encoding/json"
	"errors"
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

// NewContainerFSTransferrer returns a Transferrer that handles
// ContainerPath + ReadStream/WriteStream transfer pairs.
func NewContainerFSTransferrer(bundleDir string) ctransfer.Transferrer {
	return &containerFSTransferrer{bundleDir: bundleDir}
}

type containerFSTransferrer struct {
	bundleDir string
}

func (t *containerFSTransferrer) Transfer(ctx context.Context, src, dst any, opts ...ctransfer.Opt) error {
	switch s := src.(type) {
	case *ContainerPath:
		// Copy-from: ContainerPath -> WriteStream
		d, ok := dst.(*WriteStream)
		if !ok {
			return errdefs.ErrNotImplemented
		}
		bundle := filepath.Join(t.bundleDir, s.ContainerID)
		root, src, err := resolveMountRoot(bundle, s.Path)
		if err != nil {
			return err
		}
		w := d.Writer(ctx)
		defer w.Close()
		return writePath(root, src, w, d.MediaType, s.NoWalk)

	case *ReadStream:
		// Copy-to: ReadStream -> ContainerPath
		d, ok := dst.(*ContainerPath)
		if !ok {
			return errdefs.ErrNotImplemented
		}
		bundle := filepath.Join(t.bundleDir, d.ContainerID)
		root, dst, err := resolveMountRoot(bundle, d.Path)
		if err != nil {
			return err
		}
		r := s.Reader(ctx)
		return readPath(r, root, dst, s.MediaType, d.PreserveOwnership)
	}

	return errdefs.ErrNotImplemented
}

// resolveMountRoot maps a path expressed in the container's view onto the
// directory that backs it, returning that directory and the path relative to
// it.
//
// The bundle's rootfs backs only the paths no mount covers. Where the runtime
// spec declares a bind mount, the container's mount namespace has the source
// mounted over the destination, so the rootfs entry underneath is shadowed:
// extracting there produces a file the container never sees, and archiving
// from there reads whatever the rootfs happens to hold rather than the mounted
// content. Resolving against the mount's source keeps both directions
// consistent with the container's own view of its filesystem.
//
// The longest matching destination wins, so a mount nested inside another
// resolves against the innermost one. A bundle with no readable or parseable
// config.json resolves to the rootfs: absent mount information there is
// nothing to redirect, and the caller reports any genuine failure.
func resolveMountRoot(bundleContainerDir, containerPath string) (root, rel string, err error) {
	rootfs := filepath.Join(bundleContainerDir, "rootfs")

	data, err := os.ReadFile(filepath.Join(bundleContainerDir, "config.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rootfs, containerPath, nil
		}
		return "", "", fmt.Errorf("failed to read bundle config: %w", err)
	}

	var spec struct {
		Mounts []struct {
			Destination string `json:"destination"`
			Type        string `json:"type"`
			Source      string `json:"source"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return rootfs, containerPath, nil
	}

	target := path.Clean("/" + containerPath)

	var bestDest, bestSrc string
	for _, m := range spec.Mounts {
		if m.Type != "bind" || m.Source == "" {
			continue
		}
		dest := path.Clean("/" + m.Destination)
		if target != dest && !strings.HasPrefix(target, strings.TrimSuffix(dest, "/")+"/") {
			continue
		}
		if len(dest) > len(bestDest) {
			bestDest, bestSrc = dest, m.Source
		}
	}
	if bestDest == "" {
		return rootfs, containerPath, nil
	}

	rel = strings.TrimPrefix(target, bestDest)
	if rel == "" {
		rel = "."
	}
	return bestSrc, rel, nil
}

// rootRel converts a path expressed in the container's view (which
// may be absolute or contain parent-directory components) into a path
// usable with *os.Root operations. Leading "/" is stripped after
// cleaning, and any leading "../" sequences are collapsed by Clean,
// guaranteeing the result resolves within the root. An empty result
// is mapped to ".", which os.Root treats as the root itself.
func rootRel(p string) string {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	if p == "" {
		return "."
	}
	return p
}

// writePath creates a tar archive from the given path within rootfs
// and writes it to w. When noWalk is true and path is a directory,
// only the directory entry itself is included without walking into
// it.
//
// All filesystem accesses are anchored to rootfs through *os.Root,
// so symlink resolution cannot escape the rootfs even if the
// container concurrently mutates its own filesystem.
func writePath(rootfs, src string, w io.Writer, mediaType string, noWalk bool) error {
	if mediaType != mediaTypeTar {
		return fmt.Errorf("unsupported media type %q: %w", mediaType, errdefs.ErrNotImplemented)
	}

	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return fmt.Errorf("failed to open rootfs: %w", err)
	}
	defer root.Close()

	relPath := rootRel(src)

	fi, err := root.Lstat(relPath)
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", src, err)
	}

	// The top-level entry name is the basename of the requested
	// path. When the caller asks for the whole filesystem (path "/"),
	// relPath is "." and baseName is "."; child entries then drop
	// the leading "./" via path.Join, so the tar contains
	// "bin/sh" rather than leaking the host bundle's directory name.
	baseName := path.Base(relPath)

	tw := tar.NewWriter(w)

	if !fi.IsDir() || noWalk {
		if err := writeTarEntry(root, tw, relPath, fi, baseName); err != nil {
			tw.Close()
			return err
		}
		return tw.Close()
	}

	if err := fs.WalkDir(root.FS(), relPath, func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
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
			// Walking from the rootfs root: walkPath is already the
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

// writeTarEntry writes a single tar entry. srcPath is interpreted
// relative to root, so symlink resolution cannot escape the rootfs.
func writeTarEntry(root *os.Root, tw *tar.Writer, srcPath string, fi os.FileInfo, name string) error {
	header, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	header.Name = name

	if fi.Mode()&os.ModeSymlink != 0 {
		link, err := root.Readlink(srcPath)
		if err != nil {
			return err
		}
		header.Linkname = link
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	if fi.Mode().IsRegular() {
		f, err := root.Open(srcPath)
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

// readPath reads a tar archive from r and extracts it under path
// within rootfs. When preserveOwnership is true, extracted files have
// their UID/GID set from the tar headers.
//
// The destination directory is opened as a sub-*os.Root so the
// destination boundary is enforced by os.Root rather than by lexical
// path checks. Pre-existing symlinks within the rootfs, symlinks
// created by earlier entries in the same archive, absolute symlink
// targets, and tar entry names containing "../" all resolve within
// the destination's sub-root and cannot redirect writes outside it.
func readPath(r io.Reader, rootfs, dstPath, mediaType string, preserveOwnership bool) error {
	if mediaType != mediaTypeTar {
		return fmt.Errorf("unsupported media type %q: %w", mediaType, errdefs.ErrNotImplemented)
	}

	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return fmt.Errorf("failed to open rootfs: %w", err)
	}
	defer root.Close()

	relDst := rootRel(dstPath)

	dst := root
	if relDst != "." {
		if err := root.MkdirAll(relDst, 0755); err != nil {
			return fmt.Errorf("failed to create destination: %w", err)
		}
		dst, err = root.OpenRoot(relDst)
		if err != nil {
			return fmt.Errorf("failed to open destination: %w", err)
		}
		defer dst.Close()
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
		// collapse before we hand the path to dst. dst itself enforces
		// the destination boundary.
		entryName := strings.TrimPrefix(path.Clean("/"+header.Name), "/")
		if entryName == "" {
			// Names that resolve to the destination itself (e.g. "."
			// or "/") have nothing to extract.
			continue
		}

		if err := extractTarEntry(dst, entryName, header, tr, preserveOwnership); err != nil {
			return err
		}
	}
}

func extractTarEntry(dst *os.Root, target string, header *tar.Header, r io.Reader, preserveOwnership bool) error {
	switch header.Typeflag {
	case tar.TypeDir:
		if err := dst.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
			return err
		}
	case tar.TypeReg:
		if err := dst.MkdirAll(path.Dir(target), 0755); err != nil {
			return err
		}
		f, err := dst.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
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
		if err := dst.MkdirAll(path.Dir(target), 0755); err != nil {
			return err
		}
		// The symlink target string is stored verbatim. When later
		// traversed through dst it will be resolved within the
		// destination sub-root, so an absolute or "../"-laden target
		// cannot redirect reads or writes outside it.
		if err := dst.Symlink(header.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		if err := dst.MkdirAll(path.Dir(target), 0755); err != nil {
			return err
		}
		// Hardlink source names another entry in the same archive.
		// Clean it the same way as the entry name; dst.Link enforces
		// that both ends remain inside the destination sub-root.
		linkSrc := strings.TrimPrefix(path.Clean("/"+header.Linkname), "/")
		if err := dst.Link(linkSrc, target); err != nil {
			return err
		}
	}

	if preserveOwnership {
		if err := dst.Lchown(target, header.Uid, header.Gid); err != nil {
			return fmt.Errorf("failed to chown %s: %w", target, err)
		}
	}

	return nil
}
