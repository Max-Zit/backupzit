//go:build linux

package restorer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
	"golang.org/x/sys/unix"
)

// Owner, mode, extended attributes and times are set through a handle of
// the restored entry itself (or relative to its folder without following
// it), never by path: a user who may write to the folder could otherwise
// swap the restored file for a link to /etc/shadow and have its owner and
// mode set there. The order matters: chown clears setuid bits and file
// capabilities, so mode and attributes come after it, times last.

// applyFileMeta sets the metadata of the regular file f just written.
func applyFileMeta(f *os.File, n *repo.Node, setTimes bool) []error {
	return applyFdMeta(int(f.Fd()), f.Name(), n, setTimes) // #nosec G115 -- a file descriptor
}

// afterRename: nothing is left to set on Linux.
func afterRename(*fsutil.Dir, string, *repo.Node) []error { return nil }

// applyDirMeta sets the metadata of the restored folder sub.
func applyDirMeta(_, sub *fsutil.Dir, _ string, n *repo.Node, setTimes bool) []error {
	fd, err := sub.Fd()
	if err != nil {
		return []error{err}
	}
	return applyFdMeta(fd, sub.Path(), n, setTimes)
}

// applySelfMeta sets the metadata of the folder d itself (RunDir's target).
func applySelfMeta(d *fsutil.Dir, n *repo.Node) []error { return applyDirMeta(nil, d, "", n, true) }

func applyFdMeta(fd int, p string, n *repo.Node, setTimes bool) []error {
	var errs []error
	if n.Unix != nil && os.Geteuid() == 0 {
		if err := unix.Fchown(fd, int(n.Unix.UID), int(n.Unix.GID)); err != nil {
			errs = append(errs, &fs.PathError{Op: "chown", Path: p, Err: err})
		}
	}
	if err := unix.Fchmod(fd, unixMode(fs.FileMode(n.Mode))); err != nil {
		errs = append(errs, &fs.PathError{Op: "chmod", Path: p, Err: err})
	}
	if n.Unix != nil {
		for name, val := range n.Unix.Xattrs {
			if err := unix.Fsetxattr(fd, name, val, 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
				errs = append(errs, &fs.PathError{Op: "setxattr " + name, Path: p, Err: err})
			}
		}
	}
	if setTimes && !n.ModTime.IsZero() {
		if err := fdTimes(fd, n); err != nil {
			errs = append(errs, &fs.PathError{Op: "set times", Path: p, Err: err})
		}
	}
	return errs
}

// fdTimes sets the access and modification times of fd.
func fdTimes(fd int, n *repo.Node) error {
	ts := []unix.Timespec{unix.NsecToTimespec(n.ModTime.UnixNano()), unix.NsecToTimespec(n.ModTime.UnixNano())}
	err := unix.UtimesNanoAt(fd, "", ts, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		// Kernels before 5.8: by the descriptor's own path in /proc.
		err = unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), ts, 0)
	}
	return err
}

// applyLinkMeta sets owner, extended attributes and times of the symbolic
// link name in d, through a handle of the link itself. It must still be the
// link the restore created (a symbolic link of the agent's user with one
// name): by name, a hard link to /etc/shadow put in its place would get
// the owner instead.
func applyLinkMeta(d *fsutil.Dir, name string, n *repo.Node) []error {
	dfd, err := d.Fd()
	if err != nil {
		return []error{err}
	}
	p := d.Join(name)
	fd, err := unix.Openat(dfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return []error{&fs.PathError{Op: "open", Path: p, Err: err}}
	}
	defer unix.Close(fd) //nolint:errcheck // path handle
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return []error{err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return []error{fmt.Errorf("%s changed while it was restored", p)}
	}
	// The link itself through its descriptor: the /proc entry of an
	// O_PATH|O_NOFOLLOW descriptor leads to the link, not where it points.
	self := fmt.Sprintf("/proc/self/fd/%d", fd)
	var errs []error
	if n.Unix != nil && os.Geteuid() == 0 {
		if err := unix.Fchownat(fd, "", int(n.Unix.UID), int(n.Unix.GID), unix.AT_EMPTY_PATH); err != nil {
			errs = append(errs, &fs.PathError{Op: "lchown", Path: p, Err: err})
		}
	}
	if n.Unix != nil {
		for k, val := range n.Unix.Xattrs {
			if err := unix.Setxattr(self, k, val, 0); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) {
				errs = append(errs, &fs.PathError{Op: "setxattr " + k, Path: p, Err: err})
			}
		}
	}
	if !n.ModTime.IsZero() {
		ts := []unix.Timespec{unix.NsecToTimespec(n.ModTime.UnixNano()), unix.NsecToTimespec(n.ModTime.UnixNano())}
		err := unix.UtimesNanoAt(fd, "", ts, unix.AT_EMPTY_PATH)
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			err = unix.UtimesNanoAt(unix.AT_FDCWD, self, ts, 0)
		}
		if err != nil {
			errs = append(errs, &fs.PathError{Op: "set times", Path: p, Err: err})
		}
	}
	return errs
}

// makeSpecialAt creates the device node or named pipe name in d and sets
// its metadata through a handle of the node itself.
func makeSpecialAt(d *fsutil.Dir, name string, n *repo.Node) []error {
	mode := unixMode(fs.FileMode(n.Mode))
	dev := 0
	switch n.Type {
	case repo.NodeFifo:
		mode |= unix.S_IFIFO
	case repo.NodeDev:
		if n.Unix == nil {
			return []error{errors.New("device without device number")}
		}
		if n.Unix.Char {
			mode |= unix.S_IFCHR
		} else {
			mode |= unix.S_IFBLK
		}
		dev = int(n.Unix.Rdev) // #nosec G115 -- device number as stored in the snapshot
	default:
		return []error{errors.New("not a special file")}
	}
	if err := d.Mknod(name, mode, dev); err != nil {
		return []error{err}
	}
	dfd, err := d.Fd()
	if err != nil {
		return []error{err}
	}
	// O_PATH: opening a named pipe for reading would wait for a writer.
	fd, err := unix.Openat(dfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return []error{&fs.PathError{Op: "open", Path: d.Join(name), Err: err}}
	}
	defer unix.Close(fd) //nolint:errcheck // metadata only
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return []error{err}
	}
	if st.Mode&unix.S_IFMT != mode&unix.S_IFMT || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return []error{fmt.Errorf("%s changed while it was restored", d.Join(name))}
	}
	var errs []error
	p := d.Join(name)
	if n.Unix != nil && os.Geteuid() == 0 {
		if err := unix.Fchownat(fd, "", int(n.Unix.UID), int(n.Unix.GID), unix.AT_EMPTY_PATH); err != nil {
			errs = append(errs, &fs.PathError{Op: "chown", Path: p, Err: err})
		}
	}
	// The node through its descriptor (an O_PATH descriptor takes no fchmod).
	self := fmt.Sprintf("/proc/self/fd/%d", fd)
	if err := unix.Chmod(self, unixMode(fs.FileMode(n.Mode))); err != nil {
		errs = append(errs, &fs.PathError{Op: "chmod", Path: p, Err: err})
	}
	if n.Unix != nil {
		for k, val := range n.Unix.Xattrs {
			if err := unix.Setxattr(self, k, val, 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
				errs = append(errs, &fs.PathError{Op: "setxattr " + k, Path: p, Err: err})
			}
		}
	}
	if !n.ModTime.IsZero() {
		ts := []unix.Timespec{unix.NsecToTimespec(n.ModTime.UnixNano()), unix.NsecToTimespec(n.ModTime.UnixNano())}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, self, ts, 0); err != nil {
			errs = append(errs, &fs.PathError{Op: "set times", Path: p, Err: err})
		}
	}
	return errs
}
