package imaging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// MkdirBelow creates dir like os.MkdirAll, but every folder from root down
// to dir is a real folder afterwards: a symbolic link or junction where a
// folder belongs is replaced by an empty folder (the restore fills it). A
// link put into the restore folder beforehand, by anyone who may write
// there, can thus not send restored files elsewhere with the agent's
// rights. root itself and the folders above it are taken as they are (the
// administrator chose them). With root "" (restore to the original
// location) it is os.MkdirAll.
func MkdirBelow(root, dir string) error {
	if root == "" {
		return os.MkdirAll(dir, 0o755) // #nosec G301 -- folder of restored or copied files, like the original
	}
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%s is outside the restore folder %s", dir, root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil { // #nosec G301 -- the restore folder chosen by the administrator
		return err
	}
	if rel == "." {
		return nil
	}
	p := root
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, c)
		if err := realDir(p); err != nil {
			return err
		}
	}
	return nil
}

// realDir makes sure p is a folder and not a link to one.
func realDir(p string) error {
	for range 2 {
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			err := os.Mkdir(p, 0o755) // #nosec G301 -- folder of restored or copied files, like the original
			if err == nil {
				return nil
			}
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			// Created meanwhile: look at what it is.
		case err != nil:
			return err
		case fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
			// Irregular: a junction on Windows, followed like a symbolic
			// link. Removing it removes the link, not what it points to.
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("%s is a link where a folder belongs and cannot be replaced: %w", p, err)
			}
			if err := os.Mkdir(p, 0o755); err != nil { // #nosec G301 -- folder of restored or copied files, like the original
				return err
			}
			return nil
		case fi.IsDir():
			return nil
		default:
			return fmt.Errorf("%s exists and is not a folder", p)
		}
	}
	return fmt.Errorf("%s keeps changing while it is created", p)
}

// CreateFresh creates the file path for writing. Whatever is at that name
// (a part file of an earlier attempt, or a symbolic link put there) is
// removed first and the file is created anew (O_EXCL), so a link at the
// name is never written through. An alternate data stream ("file:stream"
// on Windows) is written into its file, which must not be a link.
func CreateFresh(path string) (*os.File, error) {
	if runtime.GOOS == "windows" {
		if i := strings.IndexByte(filepath.Base(path), ':'); i >= 0 {
			base := filepath.Join(filepath.Dir(path), filepath.Base(path)[:i])
			if fi, err := os.Lstat(base); err == nil && fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				return nil, fmt.Errorf("%s is a link, its stream is not written", base)
			}
		}
	}
	_ = os.Remove(path)
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- file in the restore target chosen by the administrator
}
