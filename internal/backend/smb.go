package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cloudsoda/go-smb2"
)

// SMB stores the repository on a Windows/Samba share using SMB2/3 with
// explicit credentials, so it also works from services running as
// LocalSystem and from Linux agents.
//
// Location: smb://[domain;]user@host[:port]/share[/path]
type SMB struct {
	conn    net.Conn
	session *smb2.Session
	share   *smb2.Share
	root    string
	loc     string

	// Open handles of recently read packs: a restore reads many blocks of
	// a pack one after another, and opening and closing the file for each
	// cost two extra round trips per block.
	hmu     sync.Mutex
	handles map[string]*smbHandle
	hseq    uint64
	openRO  func(name string) (smbFile, error) // tests; nil: open on the share
}

// smbFile is what a ranged read needs of an open file.
type smbFile interface {
	io.ReaderAt
	io.Closer
	Stat() (fs.FileInfo, error)
}

func (s *SMB) openFile(name string) (smbFile, error) {
	if s.openRO != nil {
		return s.openRO(name)
	}
	f, err := s.share.Open(s.p(name))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// smbHandleCache is how many pack files stay open for ranged reads.
const smbHandleCache = 8

type smbHandle struct {
	f       smbFile
	refs    int
	used    uint64
	evicted bool // out of the cache; closed when the last read ends
}

// OpenSMB connects and mounts the share named in u.
func OpenSMB(ctx context.Context, u *url.URL, opts Options) (*SMB, error) {
	user := u.User.Username()
	if pw, ok := u.User.Password(); ok && opts.SMBPassword == "" {
		opts.SMBPassword = pw
	}
	domain := opts.SMBDomain
	if d, n, ok := strings.Cut(user, ";"); ok {
		domain, user = d, n
	}
	if user == "" {
		return nil, errors.New("smb: username missing in location (smb://user@host/share/path)")
	}
	parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)
	if parts[0] == "" {
		return nil, errors.New("smb: share missing in location (smb://user@host/share/path)")
	}
	shareName, root := parts[0], ""
	if len(parts) == 2 {
		root = strings.Trim(parts[1], "/")
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "445")
	}
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("smb: connect %s: %w", host, err)
	}
	dialer := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: opts.SMBPassword, Domain: domain}}
	session, err := dialer.DialConn(ctx, conn, u.Hostname())
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smb: login as %s: %w", user, err)
	}
	share, err := session.Mount(fmt.Sprintf(`\\%s\%s`, u.Hostname(), shareName))
	if err != nil {
		_ = session.Logoff()
		_ = conn.Close()
		return nil, fmt.Errorf("smb: open share %q: %w", shareName, err)
	}
	return &SMB{conn: conn, session: session, share: share, root: root,
		loc: fmt.Sprintf(`smb://%s@%s/%s/%s`, user, host, shareName, root)}, nil
}

// p converts a repository name to a share path with backslashes.
func (s *SMB) p(name string) string {
	return strings.ReplaceAll(path.Join(s.root, name), "/", `\`)
}

func (s *SMB) Location() string { return s.loc }

func smbNotFound(name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return err
}

func (s *SMB) Save(_ context.Context, name string, data []byte) error {
	p := s.p(name)
	dir := strings.ReplaceAll(path.Dir(path.Join(s.root, name)), "/", `\`)
	if err := s.share.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("smb mkdir %s: %w", dir, err)
	}
	tmp := p + ".tmp-" + randSuffix()
	f, err := s.share.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("smb create %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = s.share.Remove(tmp)
		return fmt.Errorf("smb write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = s.share.Remove(tmp)
		return fmt.Errorf("smb flush %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = s.share.Remove(tmp)
		return err
	}
	// Rename uses SMB ReplaceIfExists: readers see the old or new complete
	// object. Never delete the destination first (locks must stay visible).
	if err := s.share.Rename(tmp, p); err != nil {
		_ = s.share.Remove(tmp)
		return fmt.Errorf("smb rename %s: %w", p, err)
	}
	return nil
}

func (s *SMB) Load(_ context.Context, name string) ([]byte, error) {
	f, err := s.share.Open(s.p(name))
	if err != nil {
		return nil, smbNotFound(name, err)
	}
	defer f.Close()
	// Sized from the file, read with one ReadAt (go-smb2 splits it into
	// chunks of the largest size the server takes); io.ReadAll started
	// with small reads and grew its buffer step by step.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, fi.Size())
	n, err := f.ReadAt(buf, 0)
	if n == len(buf) {
		return buf, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return nil, fmt.Errorf("read %s: %w", name, err)
}

func (s *SMB) LoadRange(_ context.Context, name string, offset int64, length int) ([]byte, error) {
	h, err := s.acquire(name)
	if err != nil {
		return nil, smbNotFound(name, err)
	}
	defer s.release(h)
	f := h.f
	if offset < 0 {
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		offset += fi.Size()
	}
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if n == length {
		return buf, nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	s.forget(name) // the file may have changed or gone: open it again next time
	return nil, fmt.Errorf("read %s@%d+%d: %w", name, offset, length, err)
}

// acquire returns an open handle of name for ranged reads. Pack files are
// kept open (they never change once written); other objects are opened
// for this read only.
func (s *SMB) acquire(name string) (*smbHandle, error) {
	if !strings.HasPrefix(name, "data/") {
		f, err := s.openFile(name)
		if err != nil {
			return nil, err
		}
		return &smbHandle{f: f, refs: 1, evicted: true}, nil
	}
	s.hmu.Lock()
	if h := s.handles[name]; h != nil {
		h.refs++
		s.hseq++
		h.used = s.hseq
		s.hmu.Unlock()
		return h, nil
	}
	s.hmu.Unlock()
	f, err := s.openFile(name)
	if err != nil {
		return nil, err
	}
	s.hmu.Lock()
	if h := s.handles[name]; h != nil { // opened meanwhile by another read
		h.refs++
		s.hmu.Unlock()
		_ = f.Close()
		return h, nil
	}
	if s.handles == nil {
		s.handles = map[string]*smbHandle{}
	}
	s.hseq++
	h := &smbHandle{f: f, refs: 1, used: s.hseq}
	s.handles[name] = h
	var drop []smbFile
	for len(s.handles) > smbHandleCache {
		var oldName string
		var old *smbHandle
		for n, c := range s.handles {
			if c != h && (old == nil || c.used < old.used) {
				oldName, old = n, c
			}
		}
		if old == nil {
			break
		}
		delete(s.handles, oldName)
		old.evicted = true
		if old.refs == 0 {
			drop = append(drop, old.f)
		}
	}
	s.hmu.Unlock()
	for _, f := range drop {
		_ = f.Close()
	}
	return h, nil
}

func (s *SMB) release(h *smbHandle) {
	s.hmu.Lock()
	h.refs--
	closeIt := h.evicted && h.refs == 0
	s.hmu.Unlock()
	if closeIt {
		_ = h.f.Close()
	}
}

// forget takes name out of the handle cache; its handle is closed once no
// read uses it.
func (s *SMB) forget(name string) {
	s.hmu.Lock()
	h := s.handles[name]
	var closeIt bool
	if h != nil {
		delete(s.handles, name)
		h.evicted = true
		closeIt = h.refs == 0
	}
	s.hmu.Unlock()
	if closeIt {
		_ = h.f.Close()
	}
}

// closeHandles closes every cached handle (reads still running close
// theirs when they end).
func (s *SMB) closeHandles() {
	s.hmu.Lock()
	names := make([]string, 0, len(s.handles))
	for n := range s.handles {
		names = append(names, n)
	}
	s.hmu.Unlock()
	for _, n := range names {
		s.forget(n)
	}
}

func (s *SMB) Size(_ context.Context, name string) (int64, error) {
	fi, err := s.share.Stat(s.p(name))
	if err != nil {
		return 0, smbNotFound(name, err)
	}
	return fi.Size(), nil
}

func (s *SMB) List(_ context.Context, dir string) (out []string, err error) {
	// Fail malformed directory listings instead of letting an unexpected
	// panic in the SMB client crash the agent.
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("smb: invalid directory listing from server: %v", p)
		}
	}()
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := s.share.ReadDir(s.p(rel))
		if err != nil {
			if errors.Is(smbNotFound(rel, err), ErrNotFound) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			child := path.Join(rel, e.Name())
			if e.IsDir() {
				if err := walk(child); err != nil {
					return err
				}
				continue
			}
			if !isTempName(e.Name()) {
				out = append(out, child)
			}
		}
		return nil
	}
	return out, walk(dir)
}

func (s *SMB) Remove(_ context.Context, name string) error {
	// An open handle would keep the file (go-smb2 does not share delete).
	s.forget(name)
	return smbNotFound(name, s.share.Remove(s.p(name)))
}

// RemoveDirs removes the empty folders below the repository and the
// repository folder (see DirRemover).
func (s *SMB) RemoveDirs(_ context.Context) (err error) {
	defer func() { // go-smb2 directory listings, see List
		if p := recover(); p != nil {
			err = fmt.Errorf("smb: invalid directory listing from server: %v", p)
		}
	}()
	var walk func(rel string)
	walk = func(rel string) {
		entries, err := s.share.ReadDir(s.p(rel))
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				walk(path.Join(rel, e.Name()))
			}
		}
		_ = s.share.Remove(s.p(rel))
	}
	walk("")
	return nil
}

func (s *SMB) Close() error {
	s.closeHandles()
	_ = s.share.Umount()
	_ = s.session.Logoff()
	return s.conn.Close()
}
