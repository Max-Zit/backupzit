package vmfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"reflect"
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
	// Perm holds the Unix permissions where the file system has them
	// (ext4, XFS); 0 when unknown (NTFS).
	Perm fs.FileMode `json:"-"`
	// ID identifies the inode or MFT record (see Visited); 0 when unknown.
	ID uint64 `json:"-"`
	// Stream names the NTFS alternate data stream the entry is ("" for
	// main data and for ext4/XFS, where ':' is part of the name).
	Stream string `json:"-"`
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
	return Entry{Name: e.Name, Path: Clean(e.Path), IsDir: e.IsDir, Size: e.Size, ModTime: e.ModTime, ID: e.ID, Stream: e.Stream}
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
		if !SafeName(d.Name()) || d.Name() == "lost+found" && dir == "/" {
			continue
		}
		e := Entry{Name: d.Name(), Path: path.Join(dir, d.Name()), IsDir: d.IsDir()}
		if fi, err := d.Info(); err == nil {
			e.Size, e.ModTime, e.Perm, e.ID = fi.Size(), fi.ModTime(), fi.Mode().Perm(), fileID(fi)
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
	return Entry{Name: path.Base(p), Path: p, IsDir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime(), Perm: fi.Mode().Perm(), ID: fileID(fi)}, nil
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
		_ = c.f.Close()
	}
	return n, err
}

// SafeName is imaging.SafeName (names read from guest file systems).
func SafeName(name string) bool { return imaging.SafeName(name) }

// MaxDepth limits how deep restores, downloads and listings walk into a
// guest file system: a crafted one may nest folders without end.
const MaxDepth = imaging.MaxDepth

// Within tells whether path p lies in folder dir.
func Within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Visited remembers the folders a walk has entered (by Entry.ID), so a
// folder that contains itself in a damaged or crafted file system is not
// walked again; see imaging.Visited. Use one per selected path.
type Visited = imaging.Visited

// LoopMessage is how a walk reports a folder it entered before.
func LoopMessage(p string) string { return imaging.LoopMessage(p) }

// fileID identifies the inode behind fi, for Visited; 0 when not known.
// The ext4 and XFS readers keep the inode only in unexported fields, which
// are read (never written) by name; TestFileIDFields checks that the names
// still exist after an update of the readers.
func fileID(fi fs.FileInfo) uint64 {
	switch id := fi.Sys().(type) {
	case uint64:
		return id
	case int64:
		return uint64(id) // #nosec G115 -- an identifier, compared only for equality
	}
	v := reflect.ValueOf(fi)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0
	}
	// ext4: the inode number (0 for the root, which it does not set).
	if f := v.FieldByName("ino"); f.IsValid() && f.CanInt() {
		return uint64(f.Int()) // #nosec G115 -- an identifier, compared only for equality
	}
	// XFS: no number, but the parsed inode core. It is the same each time
	// one inode is read and differs between two inodes (times in
	// nanoseconds, size, generation; on v5 file systems also the inode
	// number and checksum).
	if f := v.FieldByName("inode"); f.IsValid() && f.Kind() == reflect.Pointer && !f.IsNil() {
		if core := f.Elem().FieldByName("inodeCore"); core.IsValid() && core.Kind() == reflect.Struct {
			h := fnv.New64a()
			hashValue(h, core)
			if id := h.Sum64(); id != 0 {
				return id
			}
			return 1
		}
	}
	return 0
}

// hashValue feeds the numbers, strings and arrays inside v into h.
func hashValue(h hash.Hash64, v reflect.Value) {
	var b [8]byte
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			hashValue(h, v.Field(i))
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			hashValue(h, v.Index(i))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		binary.LittleEndian.PutUint64(b[:], v.Uint())
		_, _ = h.Write(b[:])
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		binary.LittleEndian.PutUint64(b[:], uint64(v.Int())) // #nosec G115 -- hashed bits
		_, _ = h.Write(b[:])
	case reflect.Bool:
		if v.Bool() {
			b[0] = 1
		}
		_, _ = h.Write(b[:1])
	case reflect.String:
		_, _ = h.Write([]byte(v.String()))
	}
}
