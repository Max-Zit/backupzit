// Package recovery is the core of the recovery environment in offline
// mode: it finds and opens repositories without the console, lists the
// disk images in them and runs image restores. The user interface sits
// on top of it; it has no user interface of its own.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// Source is where a repository is and how to get into it.
type Source struct {
	// Location as the agent CLI takes it: a folder, smb://, sftp://,
	// s3://, azure:// or hardened://.
	Location string
	Options  backend.Options
	// Password of the repository, or its recovery key.
	Password string
}

// Session is an open repository.
type Session struct {
	Source Source
	Repo   *repo.Repository
}

// ErrPassword is returned when the repository does not accept the
// password or recovery key.
var ErrPassword = errors.New("wrong password or recovery key")

// Open opens the repository of src.
func Open(ctx context.Context, src Source) (*Session, error) {
	if strings.TrimSpace(src.Location) == "" {
		return nil, errors.New("no repository location given")
	}
	be, err := backend.Open(ctx, src.Location, src.Options)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src.Location, err)
	}
	r, err := repo.Open(ctx, be, repo.Password(src.Password))
	if err != nil {
		_ = be.Close()
		if errors.Is(err, repo.ErrWrongPassword) || errors.Is(err, repo.ErrPasswordRequired) {
			return nil, fmt.Errorf("%w: %v", ErrPassword, err)
		}
		return nil, err
	}
	return &Session{Source: src, Repo: r}, nil
}

// Close closes the repository.
func (s *Session) Close() error { return s.Repo.Close() }

// ImageSnapshot is a snapshot with disk images.
type ImageSnapshot struct {
	Snapshot *repo.Snapshot
	// Guest is the virtual machine name for hypervisor backups.
	Guest string
}

// Time of the backup.
func (s ImageSnapshot) Time() time.Time { return s.Snapshot.Time }

// ImageSnapshots lists the snapshots that hold disk images, newest first.
func (s *Session) ImageSnapshots(ctx context.Context) ([]ImageSnapshot, error) {
	sns, err := s.Repo.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	var out []ImageSnapshot
	for _, sn := range sns {
		if len(sn.Images) == 0 {
			continue
		}
		is := ImageSnapshot{Snapshot: sn}
		if len(sn.Guests) > 0 {
			is.Guest = sn.Guests[0].Name
		}
		out = append(out, is)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time().After(out[j].Time()) })
	return out, nil
}

// FindLocalRepos looks for repositories in the folders below each root,
// up to depth levels deep: a folder is a repository when it has a
// "config" file and a "snapshots" folder. Unreadable folders are skipped.
func FindLocalRepos(roots []string, depth int) []string {
	var found []string
	var walk func(dir string, level int)
	walk = func(dir string, level int) {
		if isRepoDir(dir) {
			found = append(found, dir)
			return
		}
		if level >= depth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || skipDir(e.Name()) {
				continue
			}
			walk(filepath.Join(dir, e.Name()), level+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	sort.Strings(found)
	return found
}

func isRepoDir(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "config")); err != nil || fi.IsDir() {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "snapshots"))
	return err == nil && fi.IsDir()
}

// skipDir leaves out system folders that never hold a repository and
// are large to walk.
func skipDir(name string) bool {
	switch strings.ToLower(name) {
	case "windows", "program files", "program files (x86)", "programdata", "$recycle.bin",
		"system volume information", "users", "recovery", "$winreagent", "boot", "efi", "sources":
		return true
	}
	return false
}
