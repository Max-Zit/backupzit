//go:build linux

package fsutil

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// fileID identifies a file: device and inode.
type fileID struct{ dev, ino uint64 }

// FileID identifies a file (IDOf).
type FileID = fileID

// IDOf identifies the open file f.
func IDOf(f *os.File) (FileID, bool) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil { // #nosec G115 -- a file descriptor
		return FileID{}, false
	}
	return FileID{st.Dev, st.Ino}, true
}

// lstat looks at the entry name of d without following it. A link is
// opened itself (O_PATH|O_NOFOLLOW) and its owner and target are read from
// that handle, so they belong to the same link even if the name is swapped
// meanwhile.
func (d *Dir) lstat(name string) (entry, error) {
	f, err := d.handle()
	if err != nil {
		return entry{}, err
	}
	dfd := int(f.Fd()) // #nosec G115 -- a file descriptor
	var st unix.Stat_t
	if err := unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return entry{kind: entryMissing}, nil
		}
		return entry{}, &os.PathError{Op: "lstat", Path: d.Join(name), Err: err}
	}
	e := entry{id: fileID{st.Dev, st.Ino}}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		e.kind = entryDir
	case unix.S_IFLNK:
		e.kind = entryLink
		lfd, err := unix.Openat(dfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return entry{}, errChanged
			}
			return entry{}, &os.PathError{Op: "open link", Path: d.Join(name), Err: err}
		}
		defer unix.Close(lfd) //nolint:errcheck // read-only handle
		var lst unix.Stat_t
		if err := unix.Fstat(lfd, &lst); err != nil {
			return entry{}, err
		}
		if lst.Mode&unix.S_IFMT != unix.S_IFLNK || lst.Dev != st.Dev || lst.Ino != st.Ino {
			return entry{}, errChanged
		}
		// Owned by root (or the agent's user) is not enough when another
		// user may rename such a link into this folder or remove it to put
		// his own there: the folder must be theirs too and not writable by
		// others (or sticky), like the kernel's protected_symlinks rule.
		// And it must have one name only: with fs.protected_hardlinks off,
		// anyone may hard-link root's link (to /etc, say) into a sticky
		// folder such as /tmp, where it would look like root's own.
		e.trusted = trustedUID(lst.Uid) && lst.Nlink == 1 && parentProtected(dfd)
		buf := make([]byte, 4096)
		n, err := unix.Readlinkat(lfd, "", buf)
		if err != nil {
			return entry{}, &os.PathError{Op: "readlink", Path: d.Join(name), Err: err}
		}
		e.target = string(buf[:n])
	default:
		e.kind = entryOther
	}
	return e, nil
}

// rootID identifies the folder r is opened on.
func rootID(r *os.Root) (fileID, error) {
	f, err := r.Open(".")
	if err != nil {
		return fileID{}, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil { // #nosec G115 -- a file descriptor
		return fileID{}, err
	}
	return fileID{st.Dev, st.Ino}, nil
}

// Fd is the folder's file descriptor for calls relative to it (valid
// until Close).
func (d *Dir) Fd() (int, error) {
	f, err := d.handle()
	if err != nil {
		return -1, err
	}
	return int(f.Fd()), nil // #nosec G115 -- a file descriptor
}

// LinkFrom creates the hard link name in d to the file srcName in src,
// which must still be the regular file want (the one the restore wrote):
// one put in its place, such as a user's symbolic link, is not linked.
func (d *Dir) LinkFrom(src *Dir, srcName, name string, want FileID) error {
	sfd, err := src.Fd()
	if err != nil {
		return err
	}
	dfd, err := d.Fd()
	if err != nil {
		return err
	}
	fd, err := unix.Openat(sfd, srcName, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: src.Join(srcName), Err: err}
	}
	defer unix.Close(fd) //nolint:errcheck // path handle
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || (FileID{st.Dev, st.Ino}) != want {
		return fmt.Errorf("%s changed since it was restored", src.Join(srcName))
	}
	_ = d.root.Remove(name)
	// The checked file itself, through its descriptor.
	if err := unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), dfd, name, unix.AT_SYMLINK_FOLLOW); err != nil {
		return &os.LinkError{Op: "link", Old: src.Join(srcName), New: d.Join(name), Err: err}
	}
	return nil
}

// Mknod creates the device node or named pipe name (mode with its type).
func (d *Dir) Mknod(name string, mode uint32, dev int) error {
	dfd, err := d.Fd()
	if err != nil {
		return err
	}
	_ = d.root.Remove(name)
	if err := unix.Mknodat(dfd, name, mode, dev); err != nil {
		return &os.PathError{Op: "mknod", Path: d.Join(name), Err: err}
	}
	return nil
}

// ClearReadOnly: Windows only.
func (d *Dir) ClearReadOnly(string) {}

// ClearLocks: macOS only (no immutable flags are restored here).
func (d *Dir) ClearLocks(string) {}

func trustedUID(uid uint32) bool { return uid == 0 || int(uid) == os.Geteuid() }

// parentProtected reports whether only root (or the agent's user) may
// remove or add entries in the folder dfd.
func parentProtected(dfd int) bool {
	var st unix.Stat_t
	if err := unix.Fstat(dfd, &st); err != nil || !trustedUID(st.Uid) {
		return false
	}
	if st.Mode&unix.S_ISVTX != 0 {
		return true // sticky: others may not remove or rename what is not theirs
	}
	return st.Mode&0o002 == 0 && (st.Mode&0o020 == 0 || st.Gid == 0)
}
