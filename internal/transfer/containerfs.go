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
		rootfs := filepath.Join(t.bundleDir, s.ContainerID, "rootfs")
		w := d.Writer(ctx)
		defer w.Close()
		return writePath(rootfs, s.Path, w, d.MediaType, s.NoWalk)

	case *ReadStream:
		// Copy-to: ReadStream -> ContainerPath
		d, ok := dst.(*ContainerPath)
		if !ok {
			return errdefs.ErrNotImplemented
		}
		rootfs := filepath.Join(t.bundleDir, d.ContainerID, "rootfs")
		r := s.Reader(ctx)
		return readPath(r, rootfs, d.Path, s.MediaType, d.PreserveOwnership)
	}

	return errdefs.ErrNotImplemented
}

// writePath creates a tar archive from the given path within rootfs
// and writes it to w. When noWalk is true and path is a directory,
// only the directory entry itself is included without walking into it.
func writePath(rootfs, path string, w io.Writer, mediaType string, noWalk bool) error {
	if mediaType != mediaTypeTar {
		return fmt.Errorf("unsupported media type %q: %w", mediaType, errdefs.ErrNotImplemented)
	}

	srcPath := filepath.Join(rootfs, filepath.Clean("/"+path))

	fi, err := os.Lstat(srcPath)
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", path, err)
	}

	tw := tar.NewWriter(w)
	defer tw.Close()

	if !fi.IsDir() || noWalk {
		return writeTarEntry(tw, srcPath, fi, filepath.Base(srcPath))
	}

	return filepath.WalkDir(srcPath, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		// Compute relative path for the tar header
		rel, err := filepath.Rel(srcPath, filePath)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = filepath.Base(srcPath)
		} else {
			rel = filepath.Join(filepath.Base(srcPath), rel)
		}

		return writeTarEntry(tw, filePath, info, rel)
	})
}

func writeTarEntry(tw *tar.Writer, filePath string, fi os.FileInfo, name string) error {
	header, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	header.Name = name

	// Resolve symlink target
	if fi.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(filePath)
		if err != nil {
			return err
		}
		header.Linkname = link
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	if fi.Mode().IsRegular() {
		f, err := os.Open(filePath)
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

// readPath reads a tar archive from r and extracts it to the given path
// within rootfs. When preserveOwnership is true, extracted files have
// their UID/GID set from the tar headers.
func readPath(r io.Reader, rootfs, path, mediaType string, preserveOwnership bool) error {
	if mediaType != mediaTypeTar {
		return fmt.Errorf("unsupported media type %q: %w", mediaType, errdefs.ErrNotImplemented)
	}

	dstPath := filepath.Join(rootfs, filepath.Clean("/"+path))

	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}

		target := filepath.Join(dstPath, filepath.Clean("/"+header.Name))

		// Ensure the target is within the destination directory
		if !strings.HasPrefix(target, filepath.Clean(dstPath)+string(os.PathSeparator)) && target != filepath.Clean(dstPath) {
			return fmt.Errorf("tar entry %q would escape destination", header.Name)
		}

		if err := extractTarEntry(target, header, tr, preserveOwnership); err != nil {
			return err
		}
	}
}

func extractTarEntry(target string, header *tar.Header, r io.Reader, preserveOwnership bool) error {
	switch header.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
			return err
		}
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
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
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.Symlink(header.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.Link(header.Linkname, target); err != nil {
			return err
		}
	}

	if preserveOwnership {
		if err := os.Lchown(target, header.Uid, header.Gid); err != nil {
			return fmt.Errorf("failed to chown %s: %w", target, err)
		}
	}

	return nil
}
