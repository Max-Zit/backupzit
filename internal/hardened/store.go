// Package hardened implements the backupzit hardened repository: a small
// Linux service that stores repositories for agents over HTTPS and never
// lets a client destroy data before its immutability period ends.
//
//   - Files are write-once: an existing file is never overwritten.
//   - Every file (except repository lock files) gets the filesystem
//     immutable attribute ("chattr +i") and a retain-until time, stored as
//     its modification time. The period is set on the server, not by clients.
//   - Deleting a file that is still retained only hides it (like an S3
//     delete marker); it is really removed once its period ends. Hidden files
//     can be read again with a point-in-time view or restored with
//     "backupzit-repo undelete".
//
// Clients (agents, the console) therefore cannot destroy backups, even when
// an attacker controls them and their credentials.
package hardened

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound    = errors.New("file not found")
	ErrExists      = errors.New("file already exists with different content")
	ErrInvalidName = errors.New("invalid file name")
)

const metaDir = ".backupzit"

// Store keeps the files below a root directory.
type Store struct {
	root     string
	lockDays int
	now      func() time.Time

	mu      sync.Mutex
	deleted map[string]int64 // hidden files -> unix time of the delete
	logPath string
	logSig  string // size/mtime of the delete log as last read or written
}

// OpenStore opens (creating if needed) the store at root. Files are
// retained for lockDays after they are written.
func OpenStore(root string, lockDays int) (*Store, error) {
	if lockDays < 1 {
		return nil, errors.New("immutability period must be at least 1 day")
	}
	if err := os.MkdirAll(filepath.Join(root, metaDir), 0o700); err != nil {
		return nil, err
	}
	s := &Store{root: root, lockDays: lockDays, now: time.Now, logPath: filepath.Join(root, metaDir, "deleted.log")}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLogLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) LockDays() int { return s.lockDays }

// CheckImmutable verifies that the immutable attribute can be set on the
// store's filesystem with the service's privileges.
func (s *Store) CheckImmutable() error {
	if !immutableSupported() {
		return nil
	}
	p := filepath.Join(s.root, metaDir, "probe")
	if err := os.WriteFile(p, []byte("probe"), 0o600); err != nil {
		return err
	}
	defer os.Remove(p)
	if err := setImmutable(p, true); err != nil {
		return fmt.Errorf("%w (the service needs CAP_LINUX_IMMUTABLE and a filesystem with the immutable attribute, e.g. ext4, XFS or btrfs)", err)
	}
	return setImmutable(p, false)
}

// ValidName reports whether name is an acceptable slash-separated path:
// no empty, "." or ".." components, no hidden components, only
// [A-Za-z0-9._-] characters.
func ValidName(name string) bool {
	if name == "" || len(name) > 1024 {
		return false
	}
	for _, c := range strings.Split(name, "/") {
		if c == "" || c[0] == '.' {
			return false
		}
		for _, r := range c {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
				return false
			}
		}
	}
	return true
}

// isLockFile reports whether name is a repository lock file
// (<repo>/locks/<id>), which clients refresh and delete freely.
func isLockFile(name string) bool {
	c := strings.Split(name, "/")
	return len(c) >= 2 && c[len(c)-2] == "locks"
}

func (s *Store) path(name string) string { return filepath.Join(s.root, filepath.FromSlash(name)) }

func (s *Store) visibleLocked(name string, asOf int64) bool {
	t, ok := s.deleted[name]
	return !ok || (asOf > 0 && t > asOf)
}

// Save stores a new file. Saving an existing file succeeds only if the
// content is identical (a retried upload); lock files may be replaced.
func (s *Store) Save(name string, r io.Reader) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	p := s.path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLogLocked()
	lock := isLockFile(name)
	if _, err := os.Lstat(p); err == nil && !lock {
		same, err := sameContent(p, h.Sum(nil))
		if err != nil {
			return err
		}
		if !same {
			return ErrExists
		}
		if _, hidden := s.deleted[name]; hidden {
			// Written again after a delete: make it visible and protect it again.
			delete(s.deleted, name)
			if err := s.appendLogLocked("U", 0, name); err != nil {
				return err
			}
			return s.retainLocked(p, s.now().Add(s.period()))
		}
		return nil
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if lock {
		return nil
	}
	// One extra day keeps a file retained for the full period after a long
	// backup that reuses it (see Keep).
	return s.retainLocked(p, s.now().Add(s.period()+24*time.Hour))
}

func (s *Store) period() time.Duration { return time.Duration(s.lockDays) * 24 * time.Hour }

// retainLocked sets the retain-until time (mtime) and the immutable flag.
func (s *Store) retainLocked(p string, until time.Time) error {
	if err := setImmutable(p, false); err != nil {
		return err
	}
	if err := os.Chtimes(p, s.now(), until); err != nil {
		return err
	}
	return setImmutable(p, true)
}

func sameContent(p string, sum []byte) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return bytes.Equal(h.Sum(nil), sum), nil
}

// Open opens a file for reading. asOf > 0 (unix seconds) also shows files
// deleted after that time.
func (s *Store) Open(name string, asOf int64) (*os.File, error) {
	if !ValidName(name) {
		return nil, ErrInvalidName
	}
	s.mu.Lock()
	s.reloadLogLocked()
	visible := s.visibleLocked(name, asOf)
	s.mu.Unlock()
	if !visible {
		return nil, ErrNotFound
	}
	f, err := os.Open(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// List returns the files below dir (recursively) as slash-separated names
// relative to the store root.
func (s *Store) List(dir string, asOf int64) ([]string, error) {
	if !ValidName(dir) {
		return nil, ErrInvalidName
	}
	s.mu.Lock()
	s.reloadLogLocked()
	s.mu.Unlock()
	var out []string
	err := filepath.WalkDir(s.path(dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vis := out[:0]
	for _, n := range out {
		if s.visibleLocked(n, asOf) {
			vis = append(vis, n)
		}
	}
	sort.Strings(vis)
	return vis, nil
}

// Remove deletes a file. Files still retained are only hidden.
func (s *Store) Remove(name string) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLogLocked()
	if !s.visibleLocked(name, 0) {
		return ErrNotFound
	}
	p := s.path(name)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if isLockFile(name) {
		return os.Remove(p)
	}
	now := s.now()
	if now.Before(fi.ModTime()) {
		s.deleted[name] = now.Unix()
		return s.appendLogLocked("D", now.Unix(), name)
	}
	if err := setImmutable(p, false); err != nil {
		return err
	}
	return os.Remove(p)
}

// Keep makes sure the named files stay retained for at least the
// immutability period from now. Extended files get 50% extra time so that
// data shared by daily backups is not touched on every run. It returns the
// number of extended files.
func (s *Store) Keep(names []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLogLocked()
	need := s.now().Add(s.period())
	n := 0
	for _, name := range names {
		if !ValidName(name) {
			return n, ErrInvalidName
		}
		if isLockFile(name) || !s.visibleLocked(name, 0) {
			continue
		}
		p := s.path(name)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return n, fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		if err != nil {
			return n, err
		}
		if !fi.ModTime().Before(need) {
			continue
		}
		if err := s.retainLocked(p, need.Add(s.period()/2)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Sweep removes hidden files whose retention ended and stale temporary
// files of interrupted uploads.
func (s *Store) Sweep() (removed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLogLocked()
	now := s.now()
	changed := false
	for name := range s.deleted {
		p := s.path(name)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			delete(s.deleted, name)
			changed = true
			continue
		}
		if err != nil || now.Before(fi.ModTime()) {
			continue
		}
		if err := setImmutable(p, false); err != nil {
			return removed, err
		}
		if err := os.Remove(p); err != nil {
			return removed, err
		}
		delete(s.deleted, name)
		changed = true
		removed++
	}
	if changed {
		if err := s.writeLogLocked(); err != nil {
			return removed, err
		}
	}
	filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
			if fi, err := d.Info(); err == nil && now.Sub(fi.ModTime()) > 24*time.Hour {
				os.Remove(p)
			}
		}
		return nil
	})
	return removed, nil
}

// Undelete makes files hidden at or after since visible again and returns
// how many there were.
func (s *Store) Undelete(since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLogLocked()
	n := 0
	for name, t := range s.deleted {
		if t >= since.Unix() {
			delete(s.deleted, name)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.writeLogLocked()
}

// Stats summarizes the store.
type Stats struct {
	Files, Hidden int
	Bytes         int64
	HiddenBytes   int64
}

func (s *Store) Stats() (Stats, error) {
	s.mu.Lock()
	s.reloadLogLocked()
	hidden := make(map[string]bool, len(s.deleted))
	for n := range s.deleted {
		hidden[n] = true
	}
	s.mu.Unlock()
	var st Stats
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == metaDir {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.root, p)
		if hidden[filepath.ToSlash(rel)] {
			st.Hidden++
			st.HiddenBytes += fi.Size()
		} else {
			st.Files++
			st.Bytes += fi.Size()
		}
		return nil
	})
	return st, err
}

// ---- delete log: "D <unix> <name>" hides, "U 0 <name>" shows again.

func (s *Store) logSignature() string {
	fi, err := os.Stat(s.logPath)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
}

// reloadLogLocked picks up changes made by another process
// ("backupzit-repo undelete" while the service runs).
func (s *Store) reloadLogLocked() {
	if s.logSignature() != s.logSig {
		s.loadLogLocked()
	}
}

func (s *Store) loadLogLocked() error {
	s.deleted = map[string]int64{}
	f, err := os.Open(s.logPath)
	if errors.Is(err, fs.ErrNotExist) {
		s.logSig = ""
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), " ", 3)
		if len(parts) != 3 {
			continue
		}
		t, _ := strconv.ParseInt(parts[1], 10, 64)
		switch parts[0] {
		case "D":
			s.deleted[parts[2]] = t
		case "U":
			delete(s.deleted, parts[2])
		}
	}
	s.logSig = s.logSignature()
	return sc.Err()
}

func (s *Store) appendLogLocked(op string, t int64, name string) error {
	f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s %d %s\n", op, t, name)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	s.logSig = s.logSignature()
	return err
}

func (s *Store) writeLogLocked() error {
	var b strings.Builder
	names := make([]string, 0, len(s.deleted))
	for n := range s.deleted {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "D %d %s\n", s.deleted[n], n)
	}
	tmp := s.logPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.logPath); err != nil {
		return err
	}
	s.logSig = s.logSignature()
	return nil
}
