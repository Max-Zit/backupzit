package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dir is a folder opened for restoring into it. Every operation on its
// entries goes through the opened folder (an os.Root), never through its
// path again, and cannot lead outside it: a folder above it that is
// swapped for a link after it was opened changes nothing, and a link put
// in its place by a user who may write there is not followed out of it.
//
// Restores used to check a path and then write to it by name; a user who
// could write to a folder of the path (his profile, a folder below the
// restore folder) could swap it for a link between the two and make the
// agent (SYSTEM or root) write the restored files, their owner and their
// permissions somewhere else.
type Dir struct {
	root *os.Root
	path string
	self *os.File // the folder itself, for calls relative to it (lazy)
}

// maxLinks bounds the links followed while opening one path.
const maxLinks = 40

// trustLink decides on a link whose owner was checked (a variable for the
// tests: links made by a test belong to its user).
var trustLink = func(trusted bool) bool { return trusted }

// errChanged: the entry changed while it was looked at.
var errChanged = errors.New("changed while it was opened")

// entryKind is what a folder entry is, without following it.
type entryKind int

const (
	entryMissing entryKind = iota
	entryDir
	entryLink // symbolic link or junction
	entryOther
)

// entry describes a folder entry as it is (not followed).
type entry struct {
	kind entryKind
	id   fileID
	// For links: whether it belongs to the system or an administrator
	// (see TrustedLink), and where it points (read from the link itself).
	trusted bool
	target  string
}

// OpenFolder opens the restore folder p, creating it. It and the folders
// above it are taken as they are: the administrator chose them.
func OpenFolder(p string) (*Dir, error) {
	p = filepath.Clean(p)
	if err := os.MkdirAll(p, 0o755); err != nil { // #nosec G301 -- the restore folder chosen by the administrator
		return nil, err
	}
	r, err := os.OpenRoot(p)
	if err != nil {
		return nil, err
	}
	return &Dir{root: r, path: p}, nil
}

// OpenOriginal opens the folder p at the original location, creating what
// is missing. Every part of the path is opened from the volume root down
// through the part above it; a link on the way is followed only when it
// belongs to the system or an administrator (owner and target read from
// the link itself), never one of another user.
func OpenOriginal(p string) (*Dir, error) { return openOriginal(p, 0, true) }

// openOriginal opens p; create: missing folders are created (not on the way
// to the target of a link: a link to a folder that does not exist, such as
// one on a disk that is not mounted, is an error, as before 0.58).
func openOriginal(p string, links int, create bool) (*Dir, error) {
	if links > maxLinks {
		return nil, fmt.Errorf("%s: too many links", p)
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return nil, fmt.Errorf("%s is not an absolute path", p)
	}
	vol := volumeRoot(p)
	r, err := os.OpenRoot(vol)
	if err != nil {
		return nil, err
	}
	d := &Dir{root: r, path: vol}
	rest := strings.TrimPrefix(p, vol)
	for _, c := range strings.Split(rest, string(filepath.Separator)) {
		if c == "" {
			continue
		}
		next, err := d.sub(c, true, links, create)
		_ = d.Close()
		if err != nil {
			return nil, err
		}
		d = next
	}
	return d, nil
}

// volumeRoot returns the root of p's volume: "/", `C:\` or `\\server\share\`.
func volumeRoot(p string) string {
	v := filepath.VolumeName(p)
	if v == "" {
		return string(filepath.Separator)
	}
	return v + string(filepath.Separator)
}

// Path is the folder's path.
func (d *Dir) Path() string { return d.path }

// Join returns the path of the entry name in d.
func (d *Dir) Join(name string) string { return filepath.Join(d.path, name) }

// Root is the opened folder.
func (d *Dir) Root() *os.Root { return d.root }

// Close releases the folder.
func (d *Dir) Close() error {
	if d.self != nil {
		_ = d.self.Close()
		d.self = nil
	}
	return d.root.Close()
}

// handle is the folder itself, opened once, for calls relative to it.
func (d *Dir) handle() (*os.File, error) {
	if d.self == nil {
		f, err := d.root.Open(".")
		if err != nil {
			return nil, err
		}
		d.self = f
	}
	return d.self, nil
}

// Sub opens the folder name in d, creating it when it is missing. Below a
// restore folder (follow false) a link where the folder belongs is replaced
// by a real folder; at the original location (follow true) a link of the
// system or an administrator is followed and one of another user refused.
func (d *Dir) Sub(name string, follow bool) (*Dir, error) { return d.sub(name, follow, 0, true) }

func (d *Dir) sub(name string, follow bool, links int, create bool) (*Dir, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/`+string(filepath.Separator)) {
		return nil, fmt.Errorf("%q is not a folder name", name)
	}
	p := d.Join(name)
	for range 4 {
		e, err := d.lstat(name)
		if errors.Is(err, errChanged) {
			continue
		}
		if err != nil {
			return nil, err
		}
		switch e.kind {
		case entryMissing:
			if !create {
				return nil, fmt.Errorf("%s does not exist (a link points there)", p)
			}
			if err := d.root.Mkdir(name, 0o755); err != nil && !errors.Is(err, os.ErrExist) { // #nosec G301 -- folder of restored files, like the original
				return nil, err
			}
		case entryDir:
			r, err := d.root.OpenRoot(name)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, err
			}
			// Opened by name: it is the folder looked at only if the entry
			// there is still that folder (and not a link) now.
			id, err := rootID(r)
			if err == nil {
				var again entry
				again, err = d.lstat(name)
				if err == nil && again.kind == entryDir && again.id == id && id == e.id {
					return &Dir{root: r, path: p}, nil
				}
			}
			_ = r.Close()
			if err != nil && !errors.Is(err, errChanged) {
				return nil, err
			}
		case entryLink:
			if follow {
				if !trustLink(e.trusted) {
					return nil, fmt.Errorf("%s is a link of another user, not followed (restore into a folder instead)", p)
				}
				return openOriginal(linkTarget(d.path, e.target), links+1, false)
			}
			if err := d.root.Remove(name); err != nil {
				return nil, fmt.Errorf("%s is a link where a folder belongs and cannot be replaced: %w", p, err)
			}
		default:
			return nil, fmt.Errorf("%s exists and is not a folder", p)
		}
	}
	return nil, fmt.Errorf("%s keeps changing while it is opened", p)
}

// Lstat describes the entry name without following it.
func (d *Dir) Lstat(name string) (os.FileInfo, error) { return d.root.Lstat(name) }

// IsLink reports whether name is a symbolic link or junction (not a folder
// with another kind of reparse point, such as a cloud file placeholder).
func (d *Dir) IsLink(name string) (bool, error) {
	e, err := d.lstat(name)
	if err != nil {
		return false, err
	}
	return e.kind == entryLink, nil
}

// CreateExcl creates the file name anew for writing: whatever is at that
// name (a part file of an earlier attempt, or a link put there) is removed
// first, and the file is created exclusively, so a link is never written
// through.
func (d *Dir) CreateExcl(name string) (*os.File, error) {
	_ = d.root.Remove(name)
	return d.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// Rename moves from to to within d; something at to (an older file, or a
// link) is replaced, not followed.
func (d *Dir) Rename(from, to string) error { return d.root.Rename(from, to) }

// Remove removes the entry name (a link itself, not what it points to).
func (d *Dir) Remove(name string) error { return d.root.Remove(name) }

// Symlink creates the symbolic link name pointing to target.
func (d *Dir) Symlink(target, name string) error {
	d.ClearLocks(name) // macOS: a locked entry refuses its removal
	_ = d.root.Remove(name)
	return d.root.Symlink(target, name)
}

// linkTarget returns the absolute path a link in the folder dir points to:
// target as it is when absolute, relative to dir otherwise, and on Windows
// relative to dir's volume when it starts with one backslash ("\foo").
// dir holds no links (every link on the way was replaced by its target),
// so ".." in target is resolved by name.
func linkTarget(dir, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	if filepath.Separator == '\\' && strings.HasPrefix(target, `\`) && !strings.HasPrefix(target, `\\`) {
		return filepath.Clean(filepath.VolumeName(dir) + target)
	}
	return filepath.Join(dir, target)
}
