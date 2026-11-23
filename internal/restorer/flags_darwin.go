//go:build darwin

package restorer

import (
	"fmt"
	"io/fs"
	"os"
	"time"
	"unsafe"

	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
	"golang.org/x/sys/unix"
)

// BSD file flags and creation time of macOS, restored after the times
// (immutable and append-only flags would refuse every later change, and
// setting the modification time moves an earlier creation time).

const (
	// userFlags may be set by the owner of a file.
	userFlags = unix.UF_NODUMP | unix.UF_IMMUTABLE | unix.UF_APPEND | unix.UF_OPAQUE | unix.UF_HIDDEN
	// rootFlags may be set by root only (and SF_IMMUTABLE/SF_APPEND then
	// cleared only in single-user mode, which is what they are for).
	rootFlags = unix.SF_ARCHIVED | unix.SF_IMMUTABLE | unix.SF_APPEND
	// lockFlags refuse a rename or a new hard link: a file gets them after
	// it has its final name (afterRename).
	lockFlags = unix.UF_IMMUTABLE | unix.UF_APPEND | unix.SF_IMMUTABLE | unix.SF_APPEND
)

// Not restored: UF_COMPRESSED (the backup holds the decompressed content;
// the flag without its com.apple.decmpfs data would make the file read as
// garbage, and the kernel ignores it anyway), UF_TRACKED and UF_DATAVAULT
// (kernel and entitlement managed), SF_RESTRICTED, SF_NOUNLINK and
// SF_FIRMLINK (System Integrity Protection), SF_DATALESS (iCloud
// placeholders; the backup read the downloaded content).

// wantFlags returns the flags to set on an entry whose flags are now cur,
// for the stored flags: those a restore may set (SF_* only as root) are
// taken from the backup, every other bit stays as the kernel has it.
// only limits the bits taken from the backup (lockFlags or not).
func wantFlags(stored, cur uint32, root bool, only uint32) uint32 {
	settable := uint32(userFlags)
	if root {
		settable |= rootFlags
	}
	settable &= only
	return cur&^settable | stored&settable
}

// fdFlags sets the stored file flags (only those in only) on fd. Nothing
// is called when they already are as wanted: a user restoring a file
// without flags never meets a refusal.
func fdFlags(fd int, p string, n *repo.Node, only uint32) error {
	if n.Unix == nil {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &fs.PathError{Op: "stat", Path: p, Err: err}
	}
	want := wantFlags(n.Unix.Flags, st.Flags, os.Geteuid() == 0, only)
	if want == st.Flags {
		return nil
	}
	if err := unix.Fchflags(fd, int(want)); err != nil {
		return &fs.PathError{Op: "chflags", Path: p, Err: err}
	}
	return nil
}

// crtimeBuf is the attribute buffer of setattrlist for ATTR_CMN_CRTIME.
func crtimeBuf(t time.Time) (*unix.Attrlist, []byte) {
	al := &unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME}
	ts := unix.NsecToTimespec(t.UnixNano())
	// #nosec G103 -- setattrlist takes the timespec as raw bytes
	return al, unsafe.Slice((*byte)(unsafe.Pointer(&ts)), unsafe.Sizeof(ts))
}

// fdCrtime sets the creation time of fd. Go has no fsetattrlist: the
// descriptor's /dev/fd entry is the opened file itself (as for fdTimes), a
// link's with FSOPT_NOFOLLOW, so the attribute never reaches another file
// put at the entry's name.
func fdCrtime(fd int, n *repo.Node, link bool) error {
	if n.Unix == nil || n.Unix.Birthtime.IsZero() {
		return nil
	}
	opts := 0
	if link {
		opts = unix.FSOPT_NOFOLLOW
	}
	al, buf := crtimeBuf(n.Unix.Birthtime)
	return unix.Setattrlist(fmt.Sprintf("/dev/fd/%d", fd), al, buf, opts)
}

// applyFdExtra sets the creation time, then the file flags in only, of the
// entry open as fd.
func applyFdExtra(fd int, p string, n *repo.Node, link bool, only uint32) []error {
	var errs []error
	if err := fdCrtime(fd, n, link); err != nil {
		errs = append(errs, &fs.PathError{Op: "set creation time", Path: p, Err: err})
	}
	if err := fdFlags(fd, p, n, only); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// lockAfterRename sets the immutable and append-only flags of the file
// name in d once it has its name: with them the rename from the temporary
// name would be refused. The file is opened relative to the folder without
// following a link and must be the one written (id): another file put at
// the name in between is left alone.
func lockAfterRename(d *fsutil.Dir, name string, id fsutil.FileID, n *repo.Node) []error {
	if n.Unix == nil || n.Unix.Flags&lockFlags == 0 {
		return nil
	}
	dfd, err := d.Fd()
	if err != nil {
		return []error{err}
	}
	p := d.Join(name)
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return []error{&fs.PathError{Op: "open", Path: p, Err: err}}
	}
	f := os.NewFile(uintptr(fd), p) // #nosec G115 -- a file descriptor
	defer f.Close()                 //nolint:errcheck // metadata only
	if got, ok := fsutil.IDOf(f); !ok || got != id {
		return []error{fmt.Errorf("%s changed while it was restored", p)}
	}
	if err := fdFlags(fd, p, n, lockFlags); err != nil {
		return []error{err}
	}
	return nil
}

// unlockDir clears the immutable and append-only flags of the folder d
// opened for restoring into it: a folder locked by an earlier restore (or
// in Finder) takes no new entries and lets none be removed or replaced, so
// every file of a second restore into it would fail. applyDirMeta sets the
// stored flags again after the folder's entries; relock gives the folder
// back the flags it had when its metadata is not restored (a folder only
// passed through to reach an included path, or a first level at the
// original location).
func unlockDir(d *fsutil.Dir) (relock func()) {
	noop := func() {}
	if d == nil {
		return noop
	}
	fd, err := d.Fd()
	if err != nil {
		return noop
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Flags&lockFlags == 0 {
		return noop
	}
	if unix.Fchflags(fd, int(st.Flags&^lockFlags)) != nil {
		return noop // not the owner: the entries will report the refusal
	}
	return func() {
		if fd, err := d.Fd(); err == nil {
			_ = unix.Fchflags(fd, int(st.Flags))
		}
	}
}
