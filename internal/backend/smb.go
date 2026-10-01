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
	"time"

	"github.com/hirochachacha/go-smb2"
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
	session, err := dialer.DialContext(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smb: login as %s: %w", user, err)
	}
	share, err := session.Mount(fmt.Sprintf(`\\%s\%s`, u.Hostname(), shareName))
	if err != nil {
		session.Logoff()
		conn.Close()
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
		f.Close()
		s.share.Remove(tmp)
		return fmt.Errorf("smb write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		s.share.Remove(tmp)
		return fmt.Errorf("smb flush %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		s.share.Remove(tmp)
		return err
	}
	if err := s.share.Rename(tmp, p); err != nil {
		// SMB rename does not replace. Names are content addressed (or
		// rewritten whole), so an existing target holds equivalent data.
		if _, statErr := s.share.Stat(p); statErr == nil {
			s.share.Remove(tmp)
			return nil
		}
		s.share.Remove(tmp)
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
	return io.ReadAll(f)
}

func (s *SMB) LoadRange(_ context.Context, name string, offset int64, length int) ([]byte, error) {
	f, err := s.share.Open(s.p(name))
	if err != nil {
		return nil, smbNotFound(name, err)
	}
	defer f.Close()
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
	return nil, fmt.Errorf("read %s@%d+%d: %w", name, offset, length, err)
}

func (s *SMB) Size(_ context.Context, name string) (int64, error) {
	fi, err := s.share.Stat(s.p(name))
	if err != nil {
		return 0, smbNotFound(name, err)
	}
	return fi.Size(), nil
}

func (s *SMB) List(_ context.Context, dir string) ([]string, error) {
	var out []string
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
	return smbNotFound(name, s.share.Remove(s.p(name)))
}

func (s *SMB) Close() error {
	s.share.Umount()
	s.session.Logoff()
	return s.conn.Close()
}
