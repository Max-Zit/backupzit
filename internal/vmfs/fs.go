package vmfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/masahiro331/go-ext4-filesystem/ext4"
	"github.com/masahiro331/go-xfs-filesystem/xfs"
	"github.com/max-zit/backupzit/internal/imaging"
)

// Entry is a file or folder inside a volume. Paths use "/" separators.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// FS is a read-only file system inside a backed up disk.
type FS interface {
	List(dir string) ([]Entry, error)
	Stat(p string) (Entry, error)
	ReadFile(p string) (io.Reader, int64, error)
}

// ErrNotBrowsable is returned for volumes without a supported file system.
var ErrNotBrowsable = errors.New("this volume has no file system that can be browsed (NTFS, ext2/3/4 or XFS)")

// Open opens the file system of a volume.
func Open(v Volume) (f FS, err error) {
	defer guard(&err)
	f, err = open(v)
	if err != nil {
		return nil, err
	}
	return safeFS{f}, nil
}

func open(v Volume) (FS, error) {
	switch v.FS {
	case "ntfs":
		nv, err := imaging.OpenNTFS(v.r)
		if err != nil {
			return nil, err
		}
		return ntfsFS{nv}, nil
	case "ext4":
		f, err := ext4.NewFS(*io.NewSectionReader(v.r, 0, v.Size), nil)
		if err != nil {
			return nil, fmt.Errorf("read ext file system: %w", err)
		}
		return &ioFS{fsys: f}, nil
	case "xfs":
		f, err := xfs.NewFS(*io.NewSectionReader(v.r, 0, v.Size), nil)
		if err != nil {
			return nil, fmt.Errorf("read XFS: %w", err)
		}
		return &ioFS{fsys: f}, nil
	}
	return nil, ErrNotBrowsable
}

// Clean normalises a path to "/a/b".
func Clean(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	return path.Clean("/" + p)
}

type ntfsFS struct{ v *imaging.Volume }

func toWin(p string) string { return strings.ReplaceAll(Clean(p), "/", `\`) }

func fromNTFS(e imaging.Entry) Entry {
	return Entry{Name: e.Name, Path: Clean(e.Path), IsDir: e.IsDir, Size: e.Size, ModTime: e.ModTime}
}

func (n ntfsFS) List(dir string) ([]Entry, error) {
	es, err := n.v.List(toWin(dir))
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(es))
	for _, e := range es {
		out = append(out, fromNTFS(e))
	}
	return out, nil
}

func (n ntfsFS) Stat(p string) (Entry, error) {
	e, err := n.v.Stat(toWin(p))
	return fromNTFS(e), err
}

func (n ntfsFS) ReadFile(p string) (io.Reader, int64, error) { return n.v.ReadFile(toWin(p)) }

// ioFS adapts the ext4 and XFS readers (io/fs, paths without leading "/").
type ioFS struct{ fsys fs.FS }

func rel(p string) string {
	p = strings.TrimPrefix(Clean(p), "/")
	if p == "" {
		return "."
	}
	return p
}

func (f *ioFS) List(dir string) ([]Entry, error) {
	dir = Clean(dir)
	des, err := fs.ReadDir(f.fsys, rel(dir))
	if err != nil {
		// Some readers expect "/" for the root.
		if dir == "/" {
			if d2, err2 := fs.ReadDir(f.fsys, "/"); err2 == nil {
				des, err = d2, nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	out := make([]Entry, 0, len(des))
	for _, d := range des {
		if d.Name() == "." || d.Name() == ".." || d.Name() == "lost+found" && dir == "/" {
			continue
		}
		e := Entry{Name: d.Name(), Path: path.Join(dir, d.Name()), IsDir: d.IsDir()}
		if fi, err := d.Info(); err == nil {
			e.Size, e.ModTime = fi.Size(), fi.ModTime()
			if fi.Mode()&fs.ModeSymlink != 0 {
				e.IsDir = false
				e.Size = 0
			}
		}
		if e.IsDir {
			e.Size = 0
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// linkReader reads the target of a symbolic link (the ext4 reader can).
type linkReader interface {
	ReadLink(name string) (string, error)
}

// maxLinks bounds chains of symbolic links (and loops).
const maxLinks = 16

func (f *ioFS) isLink(p string) bool {
	fi, err := fs.Stat(f.fsys, rel(p))
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

// resolve follows symbolic links inside the file system, so a link (such
// as /etc/os-release) is read as the file it points to.
func (f *ioFS) resolve(p string) (string, error) {
	p = Clean(p)
	for i := 0; i < maxLinks; i++ {
		if p == "/" || !f.isLink(p) {
			return p, nil
		}
		lr, ok := f.fsys.(linkReader)
		if !ok {
			return "", fmt.Errorf("%s is a symbolic link, which cannot be followed in this file system", p)
		}
		t, err := lr.ReadLink(rel(p))
		if err != nil {
			return "", fmt.Errorf("read the symbolic link %s: %w", p, err)
		}
		t = strings.ReplaceAll(t, `\`, "/") // the reader cleans with the OS separator
		if !strings.HasPrefix(t, "/") {
			t = path.Join(path.Dir(p), t)
		}
		p = Clean(t)
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", p)
}

// Stat describes p; for a symbolic link, the file or folder it points to.
func (f *ioFS) Stat(p string) (Entry, error) {
	p = Clean(p)
	if p == "/" {
		return Entry{Name: "/", Path: "/", IsDir: true}, nil
	}
	target, err := f.resolve(p)
	if err != nil {
		return Entry{}, err
	}
	fi, err := fs.Stat(f.fsys, rel(target))
	if err != nil {
		return Entry{}, err
	}
	return Entry{Name: path.Base(p), Path: p, IsDir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

func (f *ioFS) ReadFile(p string) (io.Reader, int64, error) {
	target, err := f.resolve(p)
	if err != nil {
		return nil, 0, err
	}
	e, err := f.Stat(target)
	if err != nil {
		return nil, 0, err
	}
	if e.IsDir {
		return nil, 0, fmt.Errorf("%s is a folder", p)
	}
	file, err := f.fsys.Open(rel(target))
	if err != nil {
		return nil, 0, err
	}
	return &closingReader{file}, e.Size, nil
}

// closingReader closes the file at EOF so callers can treat it as a reader.
type closingReader struct{ f fs.File }

func (c *closingReader) Read(b []byte) (int, error) {
	n, err := c.f.Read(b)
	if err != nil {
		c.f.Close()
	}
	return n, err
}
