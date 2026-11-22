//go:build darwin

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

// Owner, extended attributes, mode and times are set through a handle of
// the restored entry itself (a link through an O_SYMLINK handle), never by
// path, as on Linux. The order differs from Linux: macOS refuses extended
// attributes on a file whose mode leaves no write permission even through a
// writable handle, so they come before the mode; chown still comes first
// (it clears setuid bits), then times, creation time and file flags
// (flags_darwin.go).

// kernelXattrs are extended attributes macOS manages itself and refuses to
// set: com.apple.provenance names the app that created the file.
var kernelXattrs = map[string]bool{"com.apple.provenance": true}

// applyFileMeta sets the metadata of the regular file f just written, all
// but the flags that would refuse its rename to its name (afterRename).
func applyFileMeta(f *os.File, n *repo.Node, setTimes bool) []error {
	fd := int(f.Fd()) // #nosec G115 -- a file descriptor
	errs := applyFdMeta(fd, f.Name(), n, setTimes, false)
	if setTimes {
		errs = append(errs, applyFdExtra(fd, f.Name(), n, false, ^uint32(lockFlags))...)
	}
	return errs
}

// afterRename sets the immutable and append-only flags of the file name in
// d (the file id written).
func afterRename(d *fsutil.Dir, name string, id fsutil.FileID, n *repo.Node) []error {
	return lockAfterRename(d, name, id, n)
}

// applyDirMeta sets the metadata of the restored folder sub.
func applyDirMeta(_, sub *fsutil.Dir, _ string, n *repo.Node, setTimes bool) []error {
	fd, err := sub.Fd()
	if err != nil {
		return []error{err}
	}
	errs := applyFdMeta(fd, sub.Path(), n, setTimes, false)
	if setTimes {
		// After its entries: an immutable folder takes no new ones.
		errs = append(errs, applyFdExtra(fd, sub.Path(), n, false, ^uint32(0))...)
	}
	return errs
}

// applySelfMeta sets the metadata of the folder d itself (RunDir's target).
func applySelfMeta(d *fsutil.Dir, n *repo.Node) []error { return applyDirMeta(nil, d, "", n, true) }

// applyFdMeta sets owner, extended attributes, mode (not for a link) and
// times of the entry open as fd.
func applyFdMeta(fd int, p string, n *repo.Node, setTimes, link bool) []error {
	var errs []error
	if n.Unix != nil && os.Geteuid() == 0 {
		if err := unix.Fchown(fd, int(n.Unix.UID), int(n.Unix.GID)); err != nil {
			errs = append(errs, &fs.PathError{Op: "chown", Path: p, Err: err})
		}
	}
	if n.Unix != nil {
		for name, val := range n.Unix.Xattrs {
			if kernelXattrs[name] {
				continue
			}
			err := unix.Fsetxattr(fd, name, val, 0)
			if err != nil && !errors.Is(err, unix.ENOTSUP) && !(link && errors.Is(err, unix.EPERM)) {
				errs = append(errs, &fs.PathError{Op: "setxattr " + name, Path: p, Err: err})
			}
		}
	}
	if !link {
		if err := unix.Fchmod(fd, unixMode(fs.FileMode(n.Mode))); err != nil {
			errs = append(errs, &fs.PathError{Op: "chmod", Path: p, Err: err})
		}
	}
	if setTimes && !n.ModTime.IsZero() {
		if err := fdTimes(fd, n, link); err != nil {
			errs = append(errs, &fs.PathError{Op: "set times", Path: p, Err: err})
		}
	}
	return errs
}

// fdTimes sets the access and modification times of fd. macOS has no
// futimens in Go: in nanoseconds through the descriptor's /dev/fd entry
// (the opened file itself, like /proc/self/fd on Linux), in microseconds
// with futimes for a link, whose /dev/fd entry does not resolve.
func fdTimes(fd int, n *repo.Node, link bool) error {
	if !link {
		ts := []unix.Timespec{unix.NsecToTimespec(n.ModTime.UnixNano()), unix.NsecToTimespec(n.ModTime.UnixNano())}
		if unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/dev/fd/%d", fd), ts, 0) == nil {
			return nil
		}
	}
	tv := unix.NsecToTimeval(n.ModTime.UnixNano())
	return unix.Futimes(fd, []unix.Timeval{tv, tv})
}

// openRestored opens the entry name in d that the restore just created,
// without following it (a link itself, a named pipe without waiting for a
// writer), and checks that it still is that entry: of type kind, the agent
// user's own and with one name. By name, a hard link to /etc/master.passwd
// put in its place would get the owner instead.
func openRestored(d *fsutil.Dir, name string, kind uint32) (int, error) {
	dfd, err := d.Fd()
	if err != nil {
		return -1, err
	}
	p := d.Join(name)
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	switch kind {
	case unix.S_IFLNK:
		flags = unix.O_RDONLY | unix.O_SYMLINK | unix.O_CLOEXEC
	case unix.S_IFIFO:
		flags |= unix.O_NONBLOCK
	}
	fd, err := unix.Openat(dfd, name, flags, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: p, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if uint32(st.Mode)&unix.S_IFMT != kind || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%s changed while it was restored", p)
	}
	return fd, nil
}

// applyLinkMeta sets owner, extended attributes and times of the symbolic
// link name in d, through a handle of the link itself.
func applyLinkMeta(d *fsutil.Dir, name string, n *repo.Node) []error {
	fd, err := openRestored(d, name, unix.S_IFLNK)
	if err != nil {
		return []error{err}
	}
	defer unix.Close(fd) //nolint:errcheck // metadata only
	errs := applyFdMeta(fd, d.Join(name), n, true, true)
	return append(errs, applyFdExtra(fd, d.Join(name), n, true, ^uint32(0))...)
}

// makeSpecialAt creates the device node or named pipe name in d and sets
// its metadata. A named pipe is set through its own handle; a device node
// is not opened (that would talk to the device): it is checked through the
// folder and set relative to it without following it.
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
	if n.Type == repo.NodeFifo {
		fd, err := openRestored(d, name, unix.S_IFIFO)
		if err != nil {
			return []error{err}
		}
		defer unix.Close(fd) //nolint:errcheck // metadata only
		errs := applyFdMeta(fd, d.Join(name), n, true, false)
		return append(errs, applyFdExtra(fd, d.Join(name), n, false, ^uint32(0))...)
	}
	return deviceMeta(d, name, n, mode)
}

// deviceMeta sets owner, mode, extended attributes, times, creation time
// and file flags of the device node name in d, relative to the folder and
// without following it (the last ones and the attributes by path).
func deviceMeta(d *fsutil.Dir, name string, n *repo.Node, mode uint32) []error {
	dfd, err := d.Fd()
	if err != nil {
		return []error{err}
	}
	p := d.Join(name)
	var st unix.Stat_t
	if err := unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return []error{err}
	}
	if uint32(st.Mode)&unix.S_IFMT != mode&unix.S_IFMT || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return []error{fmt.Errorf("%s changed while it was restored", p)}
	}
	var errs []error
	if n.Unix != nil && os.Geteuid() == 0 {
		if err := unix.Fchownat(dfd, name, int(n.Unix.UID), int(n.Unix.GID), unix.AT_SYMLINK_NOFOLLOW); err != nil {
			errs = append(errs, &fs.PathError{Op: "chown", Path: p, Err: err})
		}
	}
	if n.Unix != nil {
		for k, val := range n.Unix.Xattrs {
			if kernelXattrs[k] {
				continue
			}
			if err := unix.Lsetxattr(p, k, val, 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
				errs = append(errs, &fs.PathError{Op: "setxattr " + k, Path: p, Err: err})
			}
		}
	}
	if err := unix.Fchmodat(dfd, name, unixMode(fs.FileMode(n.Mode)), unix.AT_SYMLINK_NOFOLLOW); err != nil {
		errs = append(errs, &fs.PathError{Op: "chmod", Path: p, Err: err})
	}
	if !n.ModTime.IsZero() {
		ts := []unix.Timespec{unix.NsecToTimespec(n.ModTime.UnixNano()), unix.NsecToTimespec(n.ModTime.UnixNano())}
		if err := unix.UtimesNanoAt(dfd, name, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			errs = append(errs, &fs.PathError{Op: "set times", Path: p, Err: err})
		}
	}
	return append(errs, pathExtra(p, n, st.Flags)...)
}
