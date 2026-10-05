package agent

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

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
	root, unmount, err := mountNAS(ctx, s, run.NAS, writable)
	if err != nil {
		return failed(fmt.Errorf("connect to the share %s: %w", run.NAS.URL, err))
	}
	defer unmount()
	paths, err := nasPaths(root, run.Paths)
	if err != nil {
		return failed(err)
	}
	return fn(paths)
}
