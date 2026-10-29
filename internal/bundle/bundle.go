// Package bundle unpacks an uploaded release (tar.gz) into a directory, refusing anything
// that is not a plain file or directory inside the bundle root.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits protect the server from archives that are small on the wire and huge on disk.
type Limits struct {
	MaxFiles int   // entries in the archive
	MaxBytes int64 // total unpacked size
}

var DefaultLimits = Limits{MaxFiles: 20000, MaxBytes: 300 << 20}

// ErrRejected marks problems with the archive itself (the caller answers "bundle rejected").
var ErrRejected = errors.New("bundle rejected")

func reject(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRejected, fmt.Sprintf(format, a...))
}

// Extract unpacks r into dir (which must exist and be empty). A single common top-level
// directory (as `git archive --prefix=app/` makes) is stripped.
func Extract(r io.Reader, dir string, lim Limits) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return reject("not gzip")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var (
		files int
		total int64
	)
	staging, err := os.MkdirTemp(dir, ".unpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return reject("bad tar: %v", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue // pax metadata (git archive writes the commit id here)
		}
		name, err := cleanName(h.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		files++
		if files > lim.MaxFiles {
			return reject("more than %d files", lim.MaxFiles)
		}
		target := filepath.Join(staging, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > lim.MaxBytes {
				return reject("unpacked size over %d MB", lim.MaxBytes>>20)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFile(target, tr, h.Size, h.Mode); err != nil {
				return err
			}
		default:
			return reject("%s: only regular files and directories are allowed", name)
		}
	}
	if files == 0 {
		return reject("empty archive")
	}

	// strip a single top-level directory
	root := staging
	if des, err := os.ReadDir(staging); err == nil && len(des) == 1 && des[0].IsDir() {
		root = filepath.Join(staging, des[0].Name())
	}
	des, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, de := range des {
		if err := os.Rename(filepath.Join(root, de.Name()), filepath.Join(dir, de.Name())); err != nil {
			return err
		}
	}
	return nil
}

func cleanName(name string) (string, error) {
	if strings.ContainsRune(name, 0) || strings.Contains(name, `\`) {
		return "", reject("bad path %q", name)
	}
	if path.IsAbs(name) {
		return "", reject("absolute path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", reject("path with .. %q", name)
		}
	}
	c := path.Clean(name)
	if c == "." {
		return "", nil
	}
	return c, nil
}

func writeFile(target string, r io.Reader, size, mode int64) error {
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return reject("duplicate entry %s", filepath.Base(target))
		}
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		f.Close()
		return reject("truncated file %s", filepath.Base(target))
	}
	return f.Close()
}
