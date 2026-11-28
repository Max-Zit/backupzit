package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/max-zit/backupzit/internal/api"
)

// NAS jobs: the agent makes the share available locally (Linux: mounted
// below nasMountDir, read-only for backups; Windows: its UNC path) and backs
// up folders inside it like local files. Snapshots record those paths, so a
// restore to the original location mounts the share again at the same
// place, writable.

// nasShare is a parsed share URL.
type nasShare struct {
	Proto string // "smb" or "nfs"
	Host  string
	Share string // SMB share name or NFS export path
	Sub   string // folder below the SMB share, slash separated
}

func parseNASShare(raw string) (nasShare, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nasShare{}, errors.New("invalid share address")
	}
	p := strings.Trim(u.Path, "/")
	// "." and ".." would leave the share or the export: the mount root
	// must stay below it (A12).
	for _, part := range strings.Split(strings.ReplaceAll(p, `\`, "/"), "/") {
		if part == "." || part == ".." {
			return nasShare{}, fmt.Errorf("invalid share address: %q is not allowed in the path", part)
		}
	}
	switch u.Scheme {
	case "smb":
		share, sub, _ := strings.Cut(p, "/")
		if share == "" {
			return nasShare{}, errors.New("the share name is missing, e.g. smb://nas01/data")
		}
		return nasShare{Proto: "smb", Host: u.Host, Share: share, Sub: sub}, nil
	case "nfs":
		if p == "" {
			return nasShare{}, errors.New("the export is missing, e.g. nfs://nas01/volume1/data")
		}
		return nasShare{Proto: "nfs", Host: u.Host, Share: "/" + p}, nil
	}
	return nasShare{}, fmt.Errorf("unsupported share type %q (smb:// or nfs://)", u.Scheme)
}

// nasPaths turns the job's folders (relative to the share) into local
// paths below root; none means the whole share.
func nasPaths(root string, rel []string) ([]string, error) {
	if len(rel) == 0 {
		return []string{root}, nil
	}
	out := make([]string, 0, len(rel))
	for _, p := range rel {
		p = strings.Trim(strings.ReplaceAll(p, `\`, "/"), "/")
		for _, part := range strings.Split(p, "/") {
			if part == ".." {
				return nil, fmt.Errorf("invalid folder %q", p)
			}
		}
		out = append(out, filepath.Join(root, filepath.FromSlash(p)))
	}
	return out, nil
}

// withNAS mounts the run's share for the duration of fn, which gets the
// local paths of the run's folders.
func (a *Agent) withNAS(ctx context.Context, run api.Run, writable bool, fn func(paths []string) api.RunResult) api.RunResult {
	s, err := parseNASShare(run.NAS.URL)
	if err != nil {
		return failed(err)
	}
	// The share itself is mounted (and shared with other runs); the run's
	// folder below it is looked up afterwards.
	whole := s
	whole.Sub = ""
	share, release, err := shareNAS(ctx, nasKey(whole), nasConfig(writable, run.NAS), func() (string, func(), error) {
		return mountNAS(ctx, whole, run.NAS, writable)
	}, func() {
		a.log.Info("waiting for another run on the same share to finish (it is connected with other credentials or for a restore)", "run", run.ID, "share", run.NAS.URL)
	})
	connectFailed := func(err error) api.RunResult {
		if hint := nasHint(err); hint != "" {
			return failed(fmt.Errorf("connect to the share %s: %s (%w)", run.NAS.URL, hint, err))
		}
		return failed(fmt.Errorf("connect to the share %s: %w", run.NAS.URL, err))
	}
	if err != nil {
		return connectFailed(err)
	}
	defer release()
	root, err := nasSubfolder(share, s.Sub)
	if err != nil {
		return connectFailed(err)
	}
	paths, err := nasPaths(root, run.Paths)
	if err != nil {
		return failed(err)
	}
	return fn(paths)
}

// nasSubfolder is the run's folder sub (slash separated) below the share
// mounted at root.
func nasSubfolder(root, sub string) (string, error) {
	if sub == "" {
		return root, nil
	}
	p := filepath.Join(root, filepath.FromSlash(sub))
	if !belowMount(p, root) {
		return "", errors.New("the folder " + sub + " is not below the share")
	}
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errors.New("the folder " + sub + " does not exist on the share")
		}
		return "", fmt.Errorf("%s is not reachable: %w", p, err)
	}
	return p, nil
}

// Runs on the same share at the same time (M-24): a share has one mount
// point (Linux) or one connection per name (Windows), and each run used to
// unmount it, or cancel the connection, when it ended, under the feet of
// the others: a backup then saw an empty folder (files missing from it) or
// failed reads. Now the runs share the mount: a run with the same mode and
// credentials joins it, and the last one to finish unmounts it; a run with
// another mode (a restore to the share mounts it writable) or other
// credentials waits until the share is free.

// nasMount is a share mounted for one or more runs.
type nasMount struct {
	cfg     [32]byte // mode and credentials it was mounted with (nasConfig)
	users   int
	ready   chan struct{} // closed when mounting ended (root, unmount, err set)
	gone    chan struct{} // closed when it is unmounted and forgotten
	root    string
	unmount func()
	err     error
}

var nasMounts = struct {
	sync.Mutex
	m map[string]*nasMount
}{m: map[string]*nasMount{}}

// nasConfig identifies how a share is mounted, without keeping the
// password itself.
func nasConfig(writable bool, cred *api.NASShare) [32]byte {
	var user, domain, pass string
	if cred != nil {
		user, domain, pass = cred.User, cred.Domain, cred.Password
	}
	return sha256.Sum256([]byte(fmt.Sprintf("%t\x00%s\x00%s\x00%s", writable, user, domain, pass)))
}

// shareNAS returns the mount of the share key for a run: a new one (mount),
// or the one other runs use when it was made the same way (cfg). A share
// mounted differently is waited for (waiting is called once). release ends
// the run's use; the last user unmounts.
func shareNAS(ctx context.Context, key string, cfg [32]byte, mount func() (string, func(), error), waiting func()) (root string, release func(), err error) {
	told := false
	for {
		nasMounts.Lock()
		m := nasMounts.m[key]
		switch {
		case m == nil:
			m = &nasMount{cfg: cfg, users: 1, ready: make(chan struct{}), gone: make(chan struct{})}
			nasMounts.m[key] = m
			nasMounts.Unlock()
			m.root, m.unmount, m.err = mount()
			if m.err != nil {
				nasMounts.Lock()
				delete(nasMounts.m, key)
				nasMounts.Unlock()
				close(m.ready)
				close(m.gone)
				return "", nil, m.err
			}
			close(m.ready)
			return m.root, m.releaser(key), nil
		case m.cfg == cfg && m.users > 0:
			m.users++
			nasMounts.Unlock()
			rel := m.releaser(key)
			select {
			case <-m.ready:
			case <-ctx.Done():
				rel()
				return "", nil, ctx.Err()
			}
			if m.err != nil {
				return "", nil, m.err // the mount failed; it is forgotten already
			}
			return m.root, rel, nil
		default:
			// Mounted differently, or being unmounted: wait until it is gone.
			gone := m.gone
			nasMounts.Unlock()
			if !told && waiting != nil {
				waiting()
				told = true
			}
			select {
			case <-gone:
			case <-ctx.Done():
				return "", nil, ctx.Err()
			}
		}
	}
}

// releaser ends one run's use of m; the last one unmounts it.
func (m *nasMount) releaser(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			nasMounts.Lock()
			m.users--
			last := m.users == 0 && nasMounts.m[key] == m
			nasMounts.Unlock()
			if !last {
				return
			}
			// Only after a successful mount can the count reach 0: the run
			// that mounts holds one use until it releases its own.
			m.unmount()
			nasMounts.Lock()
			delete(nasMounts.m, key)
			nasMounts.Unlock()
			close(m.gone)
		})
	}
}

// nasHint explains the usual mount failures (mount.cifs, mount.nfs, net
// use) in words; the original message is kept after it.
func nasHint(err error) string {
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "mount error(2)"), strings.Contains(m, "network name cannot be found"), strings.Contains(m, "system error 67"):
		return "the share does not exist on this server — check the share name"
	case strings.Contains(m, "mount error(13)"), strings.Contains(m, "logon failure"), strings.Contains(m, "system error 1326"), strings.Contains(m, "system error 86"):
		return "access denied — check the user name, password and domain of the job"
	case strings.Contains(m, "access denied by server"):
		return "the NFS export does not allow this agent's address — add it on the NAS"
	case strings.Contains(m, "mount error(112)"), strings.Contains(m, "host is down"), strings.Contains(m, "no route to host"),
		strings.Contains(m, "network path was not found"), strings.Contains(m, "system error 53"), strings.Contains(m, "connection timed out"):
		return "the server does not answer — check its address and that SMB/NFS is enabled"
	case strings.Contains(m, "wrong fs type"), strings.Contains(m, "bad option"), strings.Contains(m, "executable file not found"):
		return "this machine cannot mount the share — install cifs-utils (SMB) or nfs-common / nfs-utils (NFS)"
	}
	return ""
}

// belowMount tells whether root is the mount point or a folder below it.
func belowMount(root, mount string) bool {
	rel, err := filepath.Rel(mount, root)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
