package instantnfs

import (
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
)

// FS is the exported tree: a real directory (configuration, logs, swap and
// whatever ESXi creates) in which the flat files of the disks are served
// from their overlays.
type FS struct {
	billy.Filesystem
	mu    sync.Mutex
	disks map[string]*Overlay // clean slash path → overlay
	// Disk opens the overlay of a flat disk file on first use; nil, nil if
	// the path is not a disk.
	Disk func(name string) (*Overlay, error)
}

// NewFS serves the directory root.
func NewFS(root string, disk func(string) (*Overlay, error)) *FS {
	return &FS{Filesystem: osfs.New(root, osfs.WithBoundOS()), disks: map[string]*Overlay{}, Disk: disk}
}

func cleanName(name string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, `\`, "/")), "/")
}

func (f *FS) overlay(name string) (*Overlay, error) {
	if !strings.HasSuffix(name, "-flat.vmdk") {
		return nil, nil
	}
	name = cleanName(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.disks[name]; o != nil {
		return o, nil
	}
	if f.Disk == nil {
		return nil, nil
	}
	o, err := f.Disk(name)
	if err != nil || o == nil {
		return nil, err
	}
	f.disks[name] = o
	return o, nil
}

// Forget closes the overlays below dir (a VM that is no longer served).
func (f *FS) Forget(dir string) {
	dir = cleanName(dir) + "/"
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, o := range f.disks {
		if strings.HasPrefix(k, dir) {
			o.Close()
			delete(f.disks, k)
		}
	}
}

// Open opens a file for reading.
func (f *FS) Open(name string) (billy.File, error) { return f.OpenFile(name, os.O_RDONLY, 0) }

// OpenFile serves disks from their overlays and everything else from the
// directory.
func (f *FS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	o, err := f.overlay(name)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return f.Filesystem.OpenFile(name, flag, perm)
	}
	if flag&os.O_TRUNC != 0 {
		return nil, errors.New("disks of an instant VM cannot be truncated")
	}
	return &diskFile{name: name, o: o}, nil
}

// Remove keeps the disks: ESXi only deletes them when a VM is deleted,
// which ends the instant recovery anyway.
func (f *FS) Remove(name string) error {
	if o, _ := f.overlay(name); o != nil {
		return nil
	}
	return f.Filesystem.Remove(name)
}

// Chroot is not supported: the tree is served whole.
func (f *FS) Chroot(string) (billy.Filesystem, error) { return nil, billy.ErrNotSupported }

// Change is implemented by the directory's file system.
func (f *FS) Chmod(name string, mode os.FileMode) error {
	if c, ok := f.Filesystem.(billy.Change); ok {
		return c.Chmod(name, mode)
	}
	return nil
}

func (f *FS) Lchown(name string, uid, gid int) error { return nil }
func (f *FS) Chown(name string, uid, gid int) error  { return nil }

func (f *FS) Chtimes(name string, atime, mtime time.Time) error {
	if c, ok := f.Filesystem.(billy.Change); ok {
		return c.Chtimes(name, atime, mtime)
	}
	return nil
}

// diskFile is an open disk.
type diskFile struct {
	name string
	o    *Overlay
	pos  int64
}

func (d *diskFile) Name() string                            { return d.name }
func (d *diskFile) ReadAt(p []byte, off int64) (int, error) { return d.o.ReadAt(p, off) }
func (d *diskFile) Read(p []byte) (int, error) {
	n, err := d.o.ReadAt(p, d.pos)
	d.pos += int64(n)
	return n, err
}
func (d *diskFile) Write(p []byte) (int, error) {
	n, err := d.o.WriteAt(p, d.pos)
	d.pos += int64(n)
	return n, err
}
func (d *diskFile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		d.pos = off
	case io.SeekCurrent:
		d.pos += off
	case io.SeekEnd:
		d.pos = d.o.Size() + off
	}
	return d.pos, nil
}
func (d *diskFile) Close() error  { return nil }
func (d *diskFile) Lock() error   { return nil }
func (d *diskFile) Unlock() error { return nil }
func (d *diskFile) Sync() error   { return d.o.Sync() }
func (d *diskFile) Truncate(size int64) error {
	if size != d.o.Size() {
		return errors.New("disks of an instant VM cannot be resized")
	}
	return nil
}
