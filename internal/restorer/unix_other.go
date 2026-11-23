//go:build !linux && !windows && !darwin

package restorer

import (
	"errors"
	"io/fs"
	"os"

	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
	"golang.org/x/sys/unix"
)

// applyUnix restores owner, mode, extended attributes and times. The order
// matters: chown clears setuid bits and file capabilities, so mode and
// attributes come after it, times last.
func applyUnix(dst string, n *repo.Node, setTimes bool) []error {
	var errs []error
	symlink := n.Type == repo.NodeSymlink
	if n.Unix != nil && os.Geteuid() == 0 {
		if err := os.Lchown(dst, int(n.Unix.UID), int(n.Unix.GID)); err != nil {
			errs = append(errs, err)
		}
	}
	if !symlink {
		if err := unix.Chmod(dst, unixMode(fs.FileMode(n.Mode))); err != nil {
			errs = append(errs, err)
		}
	}
	if n.Unix != nil {
		for name, val := range n.Unix.Xattrs {
			if err := unix.Lsetxattr(dst, name, val, 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
				errs = append(errs, &fs.PathError{Op: "setxattr " + name, Path: dst, Err: err})
			}
		}
	}
	if setTimes && !n.ModTime.IsZero() {
		ts := []unix.Timespec{unix.NsecToTimespec(n.ModTime.UnixNano()), unix.NsecToTimespec(n.ModTime.UnixNano())}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, dst, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// makeSpecial creates a device node or named pipe.
func makeSpecial(dst string, n *repo.Node) error {
	_ = os.Remove(dst)
	mode := unixMode(fs.FileMode(n.Mode))
	switch n.Type {
	case repo.NodeFifo:
		return unix.Mkfifo(dst, mode)
	case repo.NodeDev:
		if n.Unix == nil {
			return errors.New("device without device number")
		}
		if n.Unix.Char {
			mode |= unix.S_IFCHR
		} else {
			mode |= unix.S_IFBLK
		}
		return unix.Mknod(dst, mode, int(n.Unix.Rdev)) // #nosec G115 -- device number as stored in the snapshot
	}
	return errors.New("not a special file")
}

// Other systems (not built for now): metadata by path, as before 0.58.

func applyFileMeta(*os.File, *repo.Node, bool) []error { return nil }

func afterRename(d *fsutil.Dir, name string, _ fsutil.FileID, n *repo.Node) []error {
	return applyUnix(d.Join(name), n, true)
}

func applyDirMeta(_, sub *fsutil.Dir, _ string, n *repo.Node, setTimes bool) []error {
	return applyUnix(sub.Path(), n, setTimes)
}

func applySelfMeta(d *fsutil.Dir, n *repo.Node) []error { return applyUnix(d.Path(), n, true) }

func applyLinkMeta(d *fsutil.Dir, name string, n *repo.Node) []error {
	return applyUnix(d.Join(name), n, true)
}

func makeSpecialAt(d *fsutil.Dir, name string, n *repo.Node) []error {
	if err := makeSpecial(d.Join(name), n); err != nil {
		return []error{err}
	}
	return applyUnix(d.Join(name), n, true)
}
