// Package logfile writes a log file that rotates by size: when it would grow
// beyond MaxSize, name.log becomes name.log.1 (older ones move up to
// name.log.<Keep>, the oldest is deleted) and a new file starts.
package logfile

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
)

// Defaults: up to 4 files of 10 MB (the current one and 3 old ones).
const (
	DefaultMaxSize = 10 << 20
	DefaultKeep    = 3
)

// File is a size-rotated log file, safe for concurrent writes.
type File struct {
	Path    string
	MaxSize int64
	Keep    int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens (or creates) the log file at path with the default limits.
func Open(path string) (*File, error) {
	l := &File{Path: path, MaxSize: DefaultMaxSize, Keep: DefaultKeep}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, os.ErrClosed
	}
	if l.size > 0 && l.size+int64(len(p)) > l.MaxSize {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *File) rotate() error {
	l.f.Close()
	os.Remove(fmt.Sprintf("%s.%d", l.Path, l.Keep))
	for i := l.Keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", l.Path, i), fmt.Sprintf("%s.%d", l.Path, i+1))
	}
	if l.Keep > 0 {
		os.Rename(l.Path, l.Path+".1")
	} else {
		os.Remove(l.Path)
	}
	return l.open()
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Tail returns up to max bytes from the end of the log, older rotated files
// first, starting at a line boundary.
func Tail(path string, keep int, max int64) ([]byte, error) {
	var parts [][]byte
	left := max
	for i := 0; i <= keep && left > 0; i++ {
		p := path
		if i > 0 {
			p = fmt.Sprintf("%s.%d", path, i)
		}
		b, err := tailFile(p, left)
		if err != nil {
			if i == 0 && !os.IsNotExist(err) {
				return nil, err
			}
			break
		}
		parts = append([][]byte{b}, parts...)
		left -= int64(len(b))
	}
	out := bytes.Join(parts, nil)
	if int64(len(out)) >= max {
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return out, nil
}

func tailFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := st.Size() - max
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}
