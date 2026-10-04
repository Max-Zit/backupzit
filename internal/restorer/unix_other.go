//go:build !windows

package restorer

import (
	"errors"
	"io/fs"
	"os"

	"github.com/max-zit/backupzit/internal/repo"
	"golang.org/x/sys/unix"
)

// unixMode converts Go file mode flags to the Unix st_mode permission bits
// including setuid, setgid and sticky.
func unixMode(m fs.FileMode) uint32 {
	mode := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= unix.S_ISUID
	}
	if m&fs.ModeSetgid != 0 {
		mode |= unix.S_ISGID
	}
	if m&fs.ModeSticky != 0 {
		mode |= unix.S_ISVTX
	}
	return mode
}

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
		return unix.Mknod(dst, mode, int(n.Unix.Rdev))
	}
	return errors.New("not a special file")
}
