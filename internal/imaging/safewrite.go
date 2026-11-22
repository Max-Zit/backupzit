package imaging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/fsutil"
)

// Target writes restored files, either below a restore folder or at the
// original location.
//
// Below a restore folder every create, rename, chmod, time change and
// removal goes through an os.Root opened on that folder, so nothing is
// written outside it: not through a symbolic link or junction put there
// beforehand by anyone who may write to the folder, and not through one
// swapped in while the restore runs (checking a path and then writing to it
// by name would leave that gap). A link where a folder belongs is replaced
// by a real folder (removing a link removes the link, not what it points
// to). The restore folder itself and the folders above it are taken as they
// are: the administrator chose them.
//
// At the original location every write goes through the folder it belongs
// in, opened from the volume root down (fsutil.OpenOriginal): links are
// followed only when they belong to the system or an administrator, and a
// folder swapped for a link after it was opened changes nothing (0.58).
type Target struct {
	dir  string   // the restore folder, "" for the original location
	root *os.Root // opened on dir
	// orig is the folder at the original location used last (origKey: as
	// asked for).
	orig    *fsutil.Dir
	origKey string
}

// OpenTarget opens the restore folder dir ("" = the original location). A
// volume root ("/", "C:\") is refused as restore folder: system links there
// (/bin, /lib on merged-/usr systems) would be replaced by empty folders.
func OpenTarget(dir string) (*Target, error) {
	if dir == "" {
		return &Target{}, nil
	}
	dir = filepath.Clean(dir)
	if filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("restore into a folder, not into the root of a volume (%s)", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- the restore folder chosen by the administrator
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Target{dir: dir, root: r}, nil
}

// Close releases the restore folder.
func (t *Target) Close() error {
	if t.orig != nil {
		_ = t.orig.Close()
		t.orig = nil
	}
	if t.root == nil {
		return nil
	}
	return t.root.Close()
}

// origDir opens the folder dir at the original location (creating it).
func (t *Target) origDir(dir string) (*fsutil.Dir, error) {
	dir = filepath.Clean(dir)
	if t.orig != nil && t.origKey == dir {
		return t.orig, nil
	}
	if t.orig != nil {
		_ = t.orig.Close()
		t.orig = nil
	}
	d, err := fsutil.OpenOriginal(dir)
	if err != nil {
		return nil, err
	}
	t.orig, t.origKey = d, dir
	return d, nil
}

// rel returns p relative to the restore folder.
func (t *Target) rel(p string) (string, error) {
	rel, err := filepath.Rel(t.dir, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s is outside the restore folder %s", p, t.dir)
	}
	return rel, nil
}

// isLink: a symbolic link, or a junction (irregular and no folder on
// Windows). Folders that carry another kind of reparse point (cloud-file
// placeholders) are folders, not links.
func isLink(fi fs.FileInfo) bool {
	m := fi.Mode()
	return m&os.ModeSymlink != 0 || (m&os.ModeIrregular != 0 && !m.IsDir())
}

// Mkdir creates the folder p and the folders above it.
func (t *Target) Mkdir(p string) error {
	if t.root == nil {
		_, err := t.origDir(p)
		return err
	}
	rel, err := t.rel(p)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	cur := ""
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, c)
		if err := t.realDir(cur); err != nil {
			return err
		}
	}
	return nil
}

// realDir makes sure the folder rel (all folders above it are real) is a
// folder and not a link.
func (t *Target) realDir(rel string) error {
	for range 2 {
		fi, err := t.root.Lstat(rel)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			err := t.root.Mkdir(rel, 0o755)
			if err == nil {
				return nil
			}
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			// Created meanwhile: look at what it is.
		case err != nil:
			return err
		case isLink(fi):
			// Irregular: a junction on Windows, followed like a symbolic
			// link.
			if err := t.root.Remove(rel); err != nil {
				return fmt.Errorf("%s is a link where a folder belongs and cannot be replaced: %w", filepath.Join(t.dir, rel), err)
			}
			if err := t.root.Mkdir(rel, 0o755); err != nil {
				return err
			}
			return nil
		case fi.IsDir():
			return nil
		default:
			return fmt.Errorf("%s exists and is not a folder", filepath.Join(t.dir, rel))
		}
	}
	return fmt.Errorf("%s keeps changing while it is created", filepath.Join(t.dir, rel))
}

// Create creates the file p anew for writing. Whatever is at that name (a
// part file of an earlier attempt, or a link put there) is removed first
// and the file is created with O_EXCL, so a link at the name is never
// written through. An alternate data stream ("file:stream" on Windows) is
// written into its file, which must not be a link.
func (t *Target) Create(p string) (*os.File, error) {
	stream := runtime.GOOS == "windows" && streamBase(p) != p
	if t.root == nil {
		base := p // a ':' is part of a name except in a Windows stream
		if stream {
			base = streamBase(p)
		}
		d, err := t.origDir(filepath.Dir(base))
		if err != nil {
			return nil, err
		}
		name := filepath.Base(base)
		if stream {
			if l, err := d.IsLink(name); err == nil && l {
				return nil, fmt.Errorf("%s is a link, its stream is not written", base)
			}
			return createStream(d.Root(), name+p[len(base):], p)
		}
		return d.CreateExcl(name)
	}
	rel, err := t.rel(p)
	if err != nil {
		return nil, err
	}
	if stream {
		return createStream(t.root, rel, filepath.Join(t.dir, rel))
	}
	_ = t.root.Remove(rel)
	return t.root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// streamBase returns the file of the stream "dir\file:stream". It works on
// the string: a one-letter file name ("d:stream") would read as a drive.
func streamBase(p string) string {
	i := strings.LastIndexAny(p, `\/`) + 1
	j := strings.IndexByte(p[i:], ':')
	if j < 0 {
		return p
	}
	return p[:i+j]
}

// origEntry opens the folder of p at the original location and returns it
// with p's name in it.
func (t *Target) origEntry(p string) (*fsutil.Dir, string, error) {
	p = filepath.Clean(p)
	d, err := t.origDir(filepath.Dir(p))
	if err != nil {
		return nil, "", err
	}
	return d, filepath.Base(p), nil
}

// Replace moves the finished part file tmp to dst; something at dst (an
// older file, or a link) is removed, not followed.
func (t *Target) Replace(tmp, dst string) error {
	if t.root == nil {
		if !strings.EqualFold(filepath.Dir(filepath.Clean(tmp)), filepath.Dir(filepath.Clean(dst))) {
			return errors.New("the part file is not in the folder of " + dst)
		}
		d, name, err := t.origEntry(dst)
		if err != nil {
			return err
		}
		_ = d.Remove(name)
		return d.Rename(filepath.Base(filepath.Clean(tmp)), name)
	}
	rt, err := t.rel(tmp)
	if err != nil {
		return err
	}
	rd, err := t.rel(dst)
	if err != nil {
		return err
	}
	_ = t.root.Remove(rd)
	return t.root.Rename(rt, rd)
}

// Remove removes p (a part file left by a failed write).
func (t *Target) Remove(p string) error {
	if t.root == nil {
		d, name, err := t.origEntry(p)
		if err != nil {
			return err
		}
		return d.Remove(name)
	}
	rel, err := t.rel(p)
	if err != nil {
		return err
	}
	return t.root.Remove(rel)
}

// at returns the opened folder p's entry is in and its name there.
func (t *Target) at(p string) (*os.Root, string, error) {
	if t.root == nil {
		d, name, err := t.origEntry(p)
		if err != nil {
			return nil, "", err
		}
		return d.Root(), name, nil
	}
	rel, err := t.rel(p)
	return t.root, rel, err
}

// Chmod sets the permissions of the folder p, through the folder opened
// (files get theirs through their own handle before Replace: by name, a
// hard link put in their place could redirect them).
func (t *Target) Chmod(p string, m fs.FileMode) error {
	root, rel, err := t.at(p)
	if err != nil {
		return err
	}
	return chmodDir(root, rel, m)
}

// Chtimes sets the times of the folder p, through the folder opened.
func (t *Target) Chtimes(p string, _, mtime time.Time) error {
	root, rel, err := t.at(p)
	if err != nil {
		return err
	}
	return chtimesDir(root, rel, mtime)
}
