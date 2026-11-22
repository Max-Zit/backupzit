//go:build !linux && !windows && !darwin

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// fileID identifies a file: device and inode.
type fileID struct{ dev, ino uint64 }

// lstat looks at the entry name of d without following it (other systems:
// through the root, the link's owner and target by name).
func (d *Dir) lstat(name string) (entry, error) {
	fi, err := d.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return entry{kind: entryMissing}, nil
	}
	if err != nil {
		return entry{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return entry{}, errors.New("no file identity")
	}
	e := entry{id: fileID{uint64(st.Dev), st.Ino}} // #nosec G115 -- device numbers
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		e.kind = entryLink
		e.trusted = st.Uid == 0 || int(st.Uid) == os.Geteuid()
		t, err := d.root.Readlink(name)
		if err != nil {
			return entry{}, err
		}
		e.target = t
	case fi.IsDir():
		e.kind = entryDir
	default:
		e.kind = entryOther
	}
	return e, nil
}

func rootID(r *os.Root) (fileID, error) {
	fi, err := r.Stat(".")
	if err != nil {
		return fileID{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, errors.New("no file identity")
	}
	return fileID{uint64(st.Dev), st.Ino}, nil // #nosec G115 -- device numbers
}

// LinkFrom: the caller restores a copy.
func (d *Dir) LinkFrom(*Dir, string, string, FileID) error { return errors.ErrUnsupported }

// FileID identifies a file (not used here).
type FileID = fileID

// IDOf: hard links are restored as copies here.
func IDOf(*os.File) (FileID, bool) { return FileID{}, false }

// ClearReadOnly: Windows only.
func (d *Dir) ClearReadOnly(string) {}
