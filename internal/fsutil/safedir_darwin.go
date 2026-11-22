//go:build darwin

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

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
	return FileID{uint64(st.Dev), st.Ino}, true // #nosec G115 -- device numbers
}

// lstat looks at the entry name of d without following it. A link is
// opened itself (O_SYMLINK) and its owner is read from that handle. macOS
// does not read a link's target through a handle, so the target is read by
// name and the entry checked to still be the opened link afterwards; it
// only matters for a trusted link, whose folder nobody else may change.
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
	e := entry{id: fileID{uint64(st.Dev), st.Ino}} // #nosec G115 -- device numbers
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		e.kind = entryDir
	case unix.S_IFLNK:
		e.kind = entryLink
		lfd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_SYMLINK|unix.O_CLOEXEC, 0)
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
		// others (or sticky).
		e.trusted = trustedUID(lst.Uid) && parentProtected(dfd)
		buf := make([]byte, 4096)
		n, err := unix.Readlinkat(dfd, name, buf)
		if err != nil {
			return entry{}, &os.PathError{Op: "readlink", Path: d.Join(name), Err: err}
		}
		var again unix.Stat_t
		if err := unix.Fstatat(dfd, name, &again, unix.AT_SYMLINK_NOFOLLOW); err != nil || again.Dev != lst.Dev || again.Ino != lst.Ino {
			return entry{}, errChanged
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
	return fileID{uint64(st.Dev), st.Ino}, nil // #nosec G115 -- device numbers
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
// which must still be the regular file want (the one the restore wrote).
// macOS links by name (no handle to link from): the new link is checked
// afterwards and removed when it is not the file the restore wrote, such
// as a user's file put in its place meanwhile.
func (d *Dir) LinkFrom(src *Dir, srcName, name string, want FileID) error {
	sfd, err := src.Fd()
	if err != nil {
		return err
	}
	dfd, err := d.Fd()
	if err != nil {
		return err
	}
	isWant := func(fd int, n string) bool {
		var st unix.Stat_t
		return unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW) == nil &&
			st.Mode&unix.S_IFMT == unix.S_IFREG && (FileID{uint64(st.Dev), st.Ino}) == want // #nosec G115 -- device numbers
	}
	if !isWant(sfd, srcName) {
		return fmt.Errorf("%s changed since it was restored", src.Join(srcName))
	}
	d.ClearLocks(name)
	_ = d.root.Remove(name)
	// Without AT_SYMLINK_FOLLOW a link put at srcName is linked itself,
	// never what it points to.
	if err := unix.Linkat(sfd, srcName, dfd, name, 0); err != nil {
		return &os.LinkError{Op: "link", Old: src.Join(srcName), New: d.Join(name), Err: err}
	}
	if !isWant(dfd, name) {
		_ = d.root.Remove(name)
		return fmt.Errorf("%s changed while it was linked", src.Join(srcName))
	}
	return nil
}

// Mknod creates the device node or named pipe name (mode with its type).
// macOS has no mknodat: the node is created by name and the caller checks
// it through the folder before setting anything on it.
func (d *Dir) Mknod(name string, mode uint32, dev int) error {
	d.ClearLocks(name) // a locked entry of an earlier restore refuses its removal
	_ = d.root.Remove(name)
	p := d.Join(name)
	var err error
	if mode&unix.S_IFMT == unix.S_IFIFO {
		err = unix.Mkfifo(p, mode&^unix.S_IFMT)
	} else {
		err = unix.Mknod(p, mode, dev)
	}
	if err != nil {
		return &os.PathError{Op: "mknod", Path: p, Err: err}
	}
	return nil
}

// lockFlags refuse that an entry is replaced by a rename or removed, and
// an immutable folder takes no new entries.
const lockFlags = unix.UF_IMMUTABLE | unix.UF_APPEND | unix.SF_IMMUTABLE | unix.SF_APPEND

// ClearReadOnly removes the immutable and append-only flags of the entry
// name (see ClearLocks), so that a restored file can replace it, as
// Windows clears the read-only attribute.
func (d *Dir) ClearReadOnly(name string) { d.ClearLocks(name) }

// ClearLocks removes the immutable and append-only flags of the entry name
// of any type, without following it, so that a second restore can remove
// and create it again (a restore sets them, Finder's "Locked" sets
// UF_IMMUTABLE on folders too). The entry is changed through its own
// handle opened relative to the folder and checked to be the entry looked
// at; a device node is never opened (that would talk to the device) and is
// changed by path without following it. The kernel only lets the owner or
// root clear the UF_ flags (SF_ ones only in single-user mode): a refusal
// is left to the removal that follows to report.
func (d *Dir) ClearLocks(name string) {
	dfd, err := d.Fd()
	if err != nil {
		return
	}
	var st unix.Stat_t
	if unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Flags&lockFlags == 0 {
		return
	}
	want := st.Flags &^ lockFlags
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFCHR, unix.S_IFBLK:
		_ = setFlagsByPath(d.Join(name), want)
		return
	case unix.S_IFLNK:
		flags = unix.O_RDONLY | unix.O_SYMLINK | unix.O_CLOEXEC
	}
	fd, err := unix.Openat(dfd, name, flags, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd) //nolint:errcheck // flags only
	var got unix.Stat_t
	if unix.Fstat(fd, &got) != nil || got.Dev != st.Dev || got.Ino != st.Ino {
		return // another entry was put at the name meanwhile
	}
	_ = unix.Fchflags(fd, int(want))
}

// setFlagsByPath sets the file flags of p without following it
// (setattrlist ATTR_CMN_FLAGS with FSOPT_NOFOLLOW).
func setFlagsByPath(p string, flags uint32) error {
	al := &unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_FLAGS}
	// #nosec G103 -- setattrlist takes the flags as raw bytes
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&flags)), unsafe.Sizeof(flags))
	return unix.Setattrlist(p, al, buf, unix.FSOPT_NOFOLLOW)
}

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
