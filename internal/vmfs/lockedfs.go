package vmfs

import (
	"io/fs"
	"sync"
)

// lockedFS lets one request at a time use an ext4 or XFS file system: they
// read the volume through one shared reader (Seek, then Read), so two
// browse or download requests of the same cached disk at once could get
// each other's blocks. Every call and every read of an opened file holds
// the lock (NTFS volumes have their own).
type lockedFS struct {
	mu   *sync.Mutex
	fsys fs.FS
}

// newLockedFS wraps f; the result reads symbolic links only when f does.
func newLockedFS(f fs.FS) fs.FS {
	l := lockedFS{mu: &sync.Mutex{}, fsys: f}
	if _, ok := f.(linkReader); ok {
		return lockedLinkFS{l}
	}
	return l
}

func (l lockedFS) Open(name string) (fs.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := l.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	return &lockedFile{mu: l.mu, f: f}, nil
}

func (l lockedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fs.ReadDir(l.fsys, name)
}

func (l lockedFS) Stat(name string) (fs.FileInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fs.Stat(l.fsys, name)
}

type lockedLinkFS struct{ lockedFS }

// ReadLink reads a symbolic link.
func (l lockedLinkFS) ReadLink(name string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lr, ok := l.fsys.(linkReader)
	if !ok {
		return "", fs.ErrInvalid
	}
	return lr.ReadLink(name)
}

type lockedFile struct {
	mu *sync.Mutex
	f  fs.File
}

func (l *lockedFile) Read(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Read(b)
}

func (l *lockedFile) Stat() (fs.FileInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Stat()
}

func (l *lockedFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

func (l *lockedFile) ReadDir(n int) ([]fs.DirEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.f.(fs.ReadDirFile)
	if !ok {
		return nil, fs.ErrInvalid
	}
	return d.ReadDir(n)
}
