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

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTP stores the repository on a remote host over SFTP.
type SFTP struct {
	conn   *ssh.Client
	client *sftp.Client
	root   string
	loc    string
}

// ErrHostKeyUnknown is returned when no host key fingerprint was pinned.
// The error message contains the fingerprint presented by the server so the
// operator can verify and pin it.
type ErrHostKeyUnknown struct{ Fingerprint, KeyType string }

func (e *ErrHostKeyUnknown) Error() string {
	return fmt.Sprintf("sftp: server host key not pinned; server presented %s key %s (verify it and pass it as the host key fingerprint)", e.KeyType, e.Fingerprint)
}

// OpenSFTP connects to sftp://user@host[:port]/path.
func OpenSFTP(ctx context.Context, u *url.URL, opts Options) (*SFTP, error) {
	user := u.User.Username()
	if user == "" {
		return nil, errors.New("sftp: username missing in url")
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "22")
	}

	var auths []ssh.AuthMethod
	if opts.SFTPKeyFile != "" {
		key, err := os.ReadFile(opts.SFTPKeyFile)
		if err != nil {
			return nil, fmt.Errorf("sftp: read key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("sftp: parse key: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if pw, ok := u.User.Password(); ok && opts.SFTPPassword == "" {
		opts.SFTPPassword = pw
	}
	if opts.SFTPPassword != "" {
		pw := opts.SFTPPassword
		auths = append(auths, ssh.Password(pw),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range ans {
					ans[i] = pw
				}
				return ans, nil
			}))
	}
	if len(auths) == 0 {
		return nil, errors.New("sftp: no password or key provided")
	}

	cfg := &ssh.ClientConfig{
		User:    user,
		Auth:    auths,
		Timeout: 30 * time.Second,
		// Prefer ed25519 so the pinned fingerprint matches what operators
		// get from "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub".
		HostKeyAlgorithms: []string{
			ssh.KeyAlgoED25519,
			ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			if opts.SFTPInsecure {
				return nil
			}
			if opts.SFTPHostKey == "" {
				return &ErrHostKeyUnknown{Fingerprint: fp, KeyType: key.Type()}
			}
			// Several fingerprints may be pinned, separated by commas.
			for _, want := range strings.Split(opts.SFTPHostKey, ",") {
				if strings.TrimSpace(want) == fp {
					return nil
				}
			}
			return fmt.Errorf("sftp: host key mismatch: expected %s, server presented %s key %s", opts.SFTPHostKey, key.Type(), fp)
		},
	}

	d := net.Dialer{Timeout: cfg.Timeout}
	nc, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("sftp: dial %s: %w", host, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(nc, host, cfg)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("sftp: ssh handshake: %w", err)
	}
	conn := ssh.NewClient(c, chans, reqs)
	client, err := sftp.NewClient(conn,
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(64))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("sftp: start subsystem: %w", err)
	}

	root := u.Path
	if root == "" {
		root = "."
	}
	// sftp://host/~/repo means relative to the home directory.
	root = strings.TrimPrefix(root, "/~/")
	return &SFTP{
		conn:   conn,
		client: client,
		root:   root,
		loc:    fmt.Sprintf("sftp://%s@%s%s", user, host, u.Path),
	}, nil
}

func (s *SFTP) p(name string) string { return path.Join(s.root, name) }

func (s *SFTP) Location() string { return s.loc }

func wrapNotFound(name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	var se *sftp.StatusError
	if errors.As(err, &se) && se.FxCode() == sftp.ErrSSHFxNoSuchFile {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return err
}

func (s *SFTP) Save(_ context.Context, name string, data []byte) error {
	p := s.p(name)
	if err := s.client.MkdirAll(path.Dir(p)); err != nil {
		return fmt.Errorf("sftp mkdir %s: %w", path.Dir(p), err)
	}
	tmp := p + ".tmp-" + randSuffix()
	f, err := s.client.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return fmt.Errorf("sftp create %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		s.client.Remove(tmp)
		return fmt.Errorf("sftp write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		s.client.Remove(tmp)
		return fmt.Errorf("sftp close %s: %w", tmp, err)
	}
	if err := s.client.PosixRename(tmp, p); err != nil {
		// Server without posix-rename: plain rename fails if target exists.
		if err2 := s.client.Rename(tmp, p); err2 != nil {
			if _, statErr := s.client.Stat(p); statErr == nil {
				// Names are content addressed (or rewritten whole), so an
				// existing target holds equivalent data.
				s.client.Remove(tmp)
				return nil
			}
			s.client.Remove(tmp)
			return fmt.Errorf("sftp rename %s: %w", p, err2)
		}
	}
	return nil
}

func (s *SFTP) Load(_ context.Context, name string) ([]byte, error) {
	f, err := s.client.Open(s.p(name))
	if err != nil {
		return nil, wrapNotFound(name, err)
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (s *SFTP) LoadRange(_ context.Context, name string, offset int64, length int) ([]byte, error) {
	f, err := s.client.Open(s.p(name))
	if err != nil {
		return nil, wrapNotFound(name, err)
	}
	defer f.Close()
	if offset < 0 {
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		offset = fi.Size() + offset
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

func (s *SFTP) Size(_ context.Context, name string) (int64, error) {
	fi, err := s.client.Stat(s.p(name))
	if err != nil {
		return 0, wrapNotFound(name, err)
	}
	return fi.Size(), nil
}

func (s *SFTP) List(_ context.Context, dir string) ([]string, error) {
	var out []string
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := s.client.ReadDir(s.p(rel))
		if err != nil {
			if errors.Is(wrapNotFound(rel, err), ErrNotFound) {
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
			if isTempName(e.Name()) {
				continue
			}
			out = append(out, child)
		}
		return nil
	}
	return out, walk(dir)
}

func (s *SFTP) Remove(_ context.Context, name string) error {
	return wrapNotFound(name, s.client.Remove(s.p(name)))
}

func (s *SFTP) Close() error {
	s.client.Close()
	return s.conn.Close()
}
