// Package recovery is the core of the recovery environment in offline
// mode: it finds and opens repositories without the console, lists the
// disk images in them and runs image restores. The user interface sits
// on top of it; it has no user interface of its own.
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// RepoID is the ID the repository must have (the "id" in its config),
	// as an access package records it; empty accepts any repository at
	// Location.
	RepoID string
}

// Session is an open repository.
type Session struct {
	Source Source
	Repo   *repo.Repository
}

// ErrPassword is returned when the repository does not accept the
// password or recovery key.
var ErrPassword = errors.New("wrong password or recovery key")

// ErrOtherRepository is returned when the repository at a location is not
// the one an access package was made for: another disk with the same
// folder (rotating USB disks), or another machine's repository that a
// drive letter shuffled in Windows PE put at the stored path.
var ErrOtherRepository = errors.New("not the repository the access package was made for")

// Open opens the repository of src. With src.RepoID set, a repository with
// another ID is closed again and refused, so a restore never reads from a
// look-alike.
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
	if id := r.Config().ID; src.RepoID != "" && id != src.RepoID {
		_ = r.Close()
		return nil, fmt.Errorf("%s: %w (the package expects repository %s, this is %s)", src.Location, ErrOtherRepository, shortRepoID(src.RepoID), shortRepoID(id))
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

// LocateLocalRepo finds a local repository whose drive letter changed: an
// access package stores the path as it was where the package was made
// (G:\backups\pc), while the recovery environment gives the same disk
// another letter. If loc is a drive path that holds no repository, the
// same path is tried below each root; the first repository found is
// returned. Anything else (a working path, a URL, a UNC path, nothing
// found) comes back unchanged. It cannot tell two repositories at the same
// path apart; LocateLocalRepoID can.
func LocateLocalRepo(loc string, roots []string) string {
	if c := localRepoCandidates(loc, roots); len(c) > 0 {
		return c[0]
	}
	return loc
}

// LocateLocalRepoID is LocateLocalRepo for a repository whose ID is known
// (an access package records it): of the repositories at loc and at the
// same path below each root, it returns the first whose config has that
// ID. Two rotating USB disks with the same folder, or another machine's
// repository that a changed drive letter put at loc, are passed over. When
// repositories are there but none has the ID, the error lists what was
// found instead; when there are none, loc comes back unchanged as with
// LocateLocalRepo. The ID in config is readable without the password but
// not protected by it, so Open compares it again after opening.
func LocateLocalRepoID(loc, id string, roots []string) (string, error) {
	var found []string
	for _, c := range localRepoCandidates(loc, roots) {
		got := repoIDAt(c)
		if got == id {
			return c, nil
		}
		found = append(found, c+" ("+shortRepoID(got)+")")
	}
	if len(found) > 0 {
		return loc, fmt.Errorf("%s: %w: the package expects repository %s, found %s", loc, ErrOtherRepository, shortRepoID(id), strings.Join(found, ", "))
	}
	return loc, nil
}

// localRepoCandidates lists the repositories a drive path may mean, in the
// order LocateLocalRepo prefers them: loc itself, then the same path below
// each root. Paths that are not on a drive letter have none.
func localRepoCandidates(loc string, roots []string) []string {
	vol := filepath.VolumeName(loc)
	if len(vol) != 2 || vol[1] != ':' {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if k := strings.ToLower(filepath.Clean(p)); !seen[k] && isRepoDir(p) {
			seen[k] = true
			out = append(out, p)
		}
	}
	add(loc)
	rest := strings.TrimLeft(loc[len(vol):], `\/`)
	for _, r := range roots {
		add(filepath.Join(r, rest))
	}
	return out
}

// repoIDAt reads the repository ID from dir's config ("" when it cannot be
// read). The config is a small JSON file; more than 64 KiB is not read.
func repoIDAt(dir string) string {
	f, err := os.Open(filepath.Join(dir, "config")) // #nosec G304 -- config of a repository folder found on a local drive
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	var cfg struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&cfg); err != nil {
		return ""
	}
	return cfg.ID
}

// shortRepoID is the start of a repository ID, enough to tell repositories
// apart in a message.
func shortRepoID(id string) string {
	switch {
	case id == "":
		return "unknown"
	case len(id) > 12:
		return id[:12]
	}
	return id
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
