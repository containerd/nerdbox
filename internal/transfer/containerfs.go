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
		root, src, _, err := resolveMountRoot(bundle, s.Path)
		if err != nil {
			return err
		}
		w := d.Writer(ctx)
		defer w.Close()
		// The archive's top-level name reflects the container's view of
		// the path: a mount source's basename need not match it.
		return writePath(root, src, path.Base(rootRel(s.Path)), w, d.MediaType, s.NoWalk)

	case *ReadStream:
		// Copy-to: ReadStream -> ContainerPath
		d, ok := dst.(*ContainerPath)
		if !ok {
			return errdefs.ErrNotImplemented
		}
		bundle := filepath.Join(t.bundleDir, d.ContainerID)
		root, dst, readonly, err := resolveMountRoot(bundle, d.Path)
		if err != nil {
			return err
		}
		if readonly {
			return fmt.Errorf("container path %q is marked read-only: %w", d.Path, errdefs.ErrPermissionDenied)
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
// resolves against the innermost one. A bundle with no config.json, or one
// that does not parse as a spec, resolves to the rootfs; a config that
// exists but cannot be read is an error rather than a blind fallback.
//
// A relative source is interpreted against the bundle directory, as the
// runtime does (nerdbox itself declares such mounts for bundle extra files
// like resolv.conf). A source that is not a directory — a single-file bind
// mount — cannot anchor an *os.Root, so it resolves to the file's parent
// directory with the file's name as the relative path.
//
// The readonly result reports that the container sees the path as read-only:
// the matched mount carries a read-only option, or no mount covers the path
// and the spec's root is read-only. Writers must honor it — resolution
// bypasses the mount namespace where MS_RDONLY is enforced.
//
// Known limitations, tracked by issue #164: a path whose subtree contains a
// mount deeper inside (e.g. archiving /etc when /etc/resolv.conf is a mount)
// resolves to the outer directory only, and non-bind mounts (tmpfs, ...)
// exist only in the container's mount namespace and cannot be resolved from
// the bundle at all.
func resolveMountRoot(bundleContainerDir, containerPath string) (root, rel string, readonly bool, err error) {
	rootfs := filepath.Join(bundleContainerDir, "rootfs")

	data, err := os.ReadFile(filepath.Join(bundleContainerDir, "config.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rootfs, containerPath, false, nil
		}
		return "", "", false, fmt.Errorf("failed to read bundle config: %w", err)
	}

	var spec struct {
		Root struct {
			Readonly bool `json:"readonly"`
		} `json:"root"`
		Mounts []struct {
			Destination string   `json:"destination"`
			Type        string   `json:"type"`
			Source      string   `json:"source"`
			Options     []string `json:"options"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return rootfs, containerPath, false, nil
	}

	target := path.Clean("/" + containerPath)

	var bestDest, bestSrc string
	var bestReadonly bool
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
			bestReadonly = readOnlyMount(m.Options)
		}
	}
	if bestDest == "" {
		return rootfs, containerPath, spec.Root.Readonly, nil
	}

	// This code runs in the Linux VM, where the two predicates agree;
	// accepting either form of absolute path keeps the unit tests, which
	// mix spec-style Linux sources with host temp directories, portable
	// to Windows hosts.
	if !filepath.IsAbs(bestSrc) && !path.IsAbs(bestSrc) {
		bestSrc = filepath.Join(bundleContainerDir, bestSrc)
	}

	rel = strings.TrimPrefix(target, bestDest)

	if fi, err := os.Stat(bestSrc); err == nil && !fi.IsDir() {
		// Single-file mount: anchor at the parent directory. A residual
		// rel below the file yields a path that fails with ENOTDIR when
		// the caller stats it, which is the honest answer.
		return filepath.Dir(bestSrc), filepath.Base(bestSrc) + rel, bestReadonly, nil
	}

	if rel == "" {
		rel = "."
	}
	return bestSrc, rel, bestReadonly, nil
}

// readOnlyMount reports whether the options mark a mount read-only; a later
// "rw" or "ro" overrides an earlier one, as in mount(8) semantics.
func readOnlyMount(options []string) bool {
	readonly := false
	for _, opt := range options {
		switch opt {
		case "ro":
			readonly = true
		case "rw":
			readonly = false
		}
	}
	return readonly
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
// and writes it to w. name is the archive's top-level entry name,
// taken from the container's view of the path: when src resolved
// through a mount, the backing file or directory's own basename may
// differ from the name the container sees. When noWalk is true and
// path is a directory, only the directory entry itself is included
// without walking into it.
//
// All filesystem accesses are anchored to rootfs through *os.Root,
// so symlink resolution cannot escape the rootfs even if the
// container concurrently mutates its own filesystem.
func writePath(rootfs, src, name string, w io.Writer, mediaType string, noWalk bool) error {
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

	// When the caller asks for the whole filesystem (path "/"), name
	// is "."; child entries then drop the leading "./" via path.Join,
	// so the tar contains "bin/sh" rather than leaking the host
	// bundle's directory name.
	baseName := name

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
		// A destination naming an existing non-directory — a plain
		// file in the rootfs, or the source of a single-file bind
		// mount after resolution — receives the archived file's bytes
		// rather than a tree extraction.
		if fi, err := root.Stat(relDst); err == nil && !fi.IsDir() {
			return extractOverFile(root, relDst, r, preserveOwnership)
		}
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

// extractOverFile extracts an archive that must carry exactly one regular
// file over an existing file destination. The payload is staged in a sibling
// first so a rejected archive leaves the destination untouched, then the
// destination is truncated in place: a new inode would leave a container
// whose mount binds this file reading the stale one.
func extractOverFile(dst *os.Root, target string, r io.Reader, preserveOwnership bool) error {
	tr := tar.NewReader(r)
	header, err := tr.Next()
	if err == io.EOF {
		return fmt.Errorf("cannot extract empty archive over file %s", target)
	}
	if err != nil {
		return fmt.Errorf("failed to read tar header: %w", err)
	}
	if header.Typeflag != tar.TypeReg {
		return fmt.Errorf("cannot extract %q over file %s: not a regular file", header.Name, target)
	}

	// A fixed name overwrites debris from an interrupted transfer.
	tmp := target + ".transfer-tmp"
	f, err := dst.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to stage %s: %w", target, err)
	}
	defer dst.Remove(tmp)
	// Copy exactly the size the header declares; the tar reader
	// bounds the entry anyway, and the explicit limit satisfies
	// gosec's decompression-bomb rule (G110).
	if _, err := io.CopyN(f, tr, header.Size); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	switch _, err := tr.Next(); {
	case err == nil:
		return fmt.Errorf("cannot extract multiple entries over file %s", target)
	case err != io.EOF:
		return fmt.Errorf("failed to read tar header: %w", err)
	}

	staged, err := dst.Open(tmp)
	if err != nil {
		return err
	}
	defer staged.Close()
	out, err := dst.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, staged); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if preserveOwnership {
		if err := dst.Lchown(target, header.Uid, header.Gid); err != nil {
			return fmt.Errorf("failed to chown %s: %w", target, err)
		}
	}
	return nil
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
