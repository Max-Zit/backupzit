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
	"path/filepath"
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

func (l *Local) path(name string) string {
	return filepath.Join(l.root, filepath.FromSlash(name))
}

func (l *Local) Location() string { return l.root }

func (l *Local) Save(_ context.Context, name string, data []byte) error {
	p := l.path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp-" + randSuffix()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (l *Local) Load(_ context.Context, name string) ([]byte, error) {
	b, err := os.ReadFile(l.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return b, err
}

func (l *Local) LoadRange(_ context.Context, name string, offset int64, length int) ([]byte, error) {
	f, err := os.Open(l.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
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
	fi, err := os.Stat(l.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (l *Local) List(_ context.Context, dir string) ([]string, error) {
	base := l.path(dir)
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
	return os.Remove(l.path(name))
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
