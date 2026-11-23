package backend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Local stores the repository in a directory on a local (or mounted) filesystem.
type Local struct {
	root string
}

// OpenLocal returns a backend rooted at dir. The directory is created on first Save.
func OpenLocal(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Local{root: abs}, nil
}

// RemoveDirs removes the empty folders below the repository and the
// repository folder (see DirRemover).
func (l *Local) RemoveDirs(_ context.Context) error {
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				walk(filepath.Join(dir, e.Name()))
			}
		}
		_ = os.Remove(dir)
	}
	walk(l.root)
	return nil
}

func (l *Local) path(name string) string {
	return filepath.Join(l.root, filepath.FromSlash(name))
}

func (l *Local) Location() string { return l.root }

// Below the repository folder only real folders and regular files are used
// (on Windows also cloud and deduplicated files, which read like regular
// ones). The repository folder itself may be a link. A symbolic link to a
// file inside the repository folder is still read, but nothing leads out of
// it: a link to a file elsewhere, a link to a folder, a junction or mount
// point, a named pipe, a socket or a device below it is left out of
// listings and refused when read or written. Someone who may write into a
// mounted repository folder could otherwise make the agent, which runs as
// root or SYSTEM, read or write files of the computer through it. Listings
// never followed links to folders, so no layout that worked changes.

// realFolders checks that the folders between the repository folder and
// name exist as real folders. A missing one is reported as ErrNotFound.
func (l *Local) realFolders(name string) error {
	dir := path.Dir(name)
	if dir == "." || dir == "/" {
		return nil
	}
	p := l.root
	for _, c := range strings.Split(dir, "/") {
		if c == "" || c == "." {
			continue
		}
		p = filepath.Join(p, c)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s: %s in the repository folder is not a folder (links, junctions and mount points are not followed)", name, c)
		}
	}
	return nil
}

// inside resolves the link p and reports whether it leads to a regular
// file inside the repository folder.
func (l *Local) inside(p string) (string, error) {
	t, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(l.root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, t)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errOutside
	}
	fi, err := os.Lstat(t)
	if err != nil {
		return "", err
	}
	if !objectFile(fi) {
		return "", errOutside
	}
	return t, nil
}

var errOutside = errors.New("a link that leads out of the repository folder or to a folder is not followed")

// object returns the file that holds the object name: its own path, or
// the file inside the repository folder its symbolic link leads to.
func (l *Local) object(name string) (string, os.FileInfo, error) {
	if err := l.realFolders(name); err != nil {
		return "", nil, err
	}
	p := l.path(name)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if err != nil {
		return "", nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		t, err := l.inside(p)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", name, err)
		}
		p = t
		if fi, err = os.Lstat(p); err != nil {
			return "", nil, err
		}
	}
	return p, fi, nil
}

// open opens the object name for reading.
func (l *Local) open(name string) (*os.File, error) {
	p, fi, err := l.object(name)
	if err != nil {
		return nil, err
	}
	if !objectFile(fi) {
		return nil, fmt.Errorf("%s: not a regular file", name)
	}
	f, err := openObject(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	// What was opened, not what the name showed a moment before.
	if fi, err = f.Stat(); err != nil || !objectFile(fi) {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("%s: not a regular file", name)
		}
		return nil, err
	}
	return f, nil
}

func (l *Local) Save(_ context.Context, name string, data []byte) error {
	p := l.path(name)
	if err := l.realFolders(name); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := l.realFolders(name); err != nil {
		return err
	}
	tmp := p + ".tmp-" + randSuffix()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- temporary file next to the object in the local repository
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (l *Local) Load(_ context.Context, name string) ([]byte, error) {
	f, err := l.open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// As os.ReadFile: one buffer of the file's size.
	size := 512
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 && int64(int(fi.Size())) == fi.Size() {
		size = int(fi.Size()) + 1
	}
	b := make([]byte, 0, size)
	for {
		n, err := f.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				return b, nil
			}
			return nil, err
		}
		if len(b) >= cap(b) {
			b = append(b, 0)[:len(b)]
		}
	}
}

func (l *Local) LoadRange(_ context.Context, name string, offset int64, length int) ([]byte, error) {
	f, err := l.open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	whence := io.SeekStart
	if offset < 0 {
		whence = io.SeekEnd
	}
	if _, err := f.Seek(offset, whence); err != nil {
		return nil, err
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("read %s@%d+%d: %w", name, offset, length, err)
	}
	return buf, nil
}

func (l *Local) Size(_ context.Context, name string) (int64, error) {
	_, fi, err := l.object(name)
	if err != nil {
		return 0, err
	}
	// A folder answers as before (with its own size); pipes, sockets and
	// devices are no objects.
	if !fi.IsDir() && !objectFile(fi) {
		return 0, fmt.Errorf("%s: not a regular file", name)
	}
	return fi.Size(), nil
}

func (l *Local) List(_ context.Context, dir string) ([]string, error) {
	base := l.path(dir)
	if err := l.realFolders(path.Join(dir, "x")); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == base {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || isTempName(d.Name()) {
			return nil
		}
		if p == base {
			return nil // the folder itself is a link (WalkDir does not follow it)
		}
		// Regular files straight from the folder listing (no extra call per
		// file in large repositories); only other entries are looked at.
		switch t := d.Type(); {
		case t.IsRegular():
		case t&fs.ModeSymlink != 0:
			if _, err := l.inside(p); err != nil {
				return nil
			}
		default:
			fi, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed meanwhile
			}
			if err != nil {
				return err
			}
			if !objectFile(fi) {
				return nil
			}
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

func (l *Local) Remove(_ context.Context, name string) error {
	if err := l.realFolders(name); err != nil {
		return err
	}
	// A link itself is removed, never what it points to.
	err := os.Remove(l.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		// Like the other backends: prune and lock removal treat a missing
		// file as already gone.
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return err
}

func (l *Local) Close() error { return nil }

func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func isTempName(n string) bool {
	return len(n) > 21 && n[len(n)-21:len(n)-16] == ".tmp-"
}
