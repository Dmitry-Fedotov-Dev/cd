//go:build unix

package engine

import (
	"io/fs"
	"os"
	"path/filepath"
)

// chownTree gives the data directory to the container user (symlinks are not followed).
func chownTree(dir string, uid, gid int) error {
	return filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}
