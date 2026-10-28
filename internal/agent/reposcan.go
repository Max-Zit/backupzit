package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// maxScanBytes keeps the result of a search under the console's limit for
// agent requests.
const maxScanBytes = 3 << 20

// repoScan lists the repositories on a storage only this machine reaches
// (a local or USB disk) and reads their backups, for Find backups in the
// console (KindRepoScan).
func (a *Agent) repoScan(ctx context.Context, run api.Run) api.RunResult {
	base := run.Repository.URL
	var res api.RunResult
	if strings.HasPrefix(base, "usb://") {
		_, loc, err := backend.ResolveUSB(base)
		if err != nil {
			return failed(err)
		}
		base = loc
	}
	be, err := backend.Open(ctx, base, backend.Options{})
	if err != nil {
		return failed(fmt.Errorf("open storage: %w", err))
	}
	dirs, err := backend.RepositoryDirs(ctx, be)
	be.Close()
	if err != nil {
		return failed(fmt.Errorf("list: %w", err))
	}
	var found []api.FoundRepo
	size, snaps := 0, 0
	for _, d := range dirs {
		f := api.FoundRepo{Dir: d}
		rs := run.Repository
		rs.URL = joinLocation(base, d)
		r, closeRepo, err := a.openRepo(ctx, rs, false)
		if err == nil {
			sns, lerr := r.ListSnapshots(ctx)
			closeRepo()
			err = lerr
			for _, sn := range sns {
				b, _ := json.Marshal(sn)
				if size += len(b); size > maxScanBytes {
					f.Error = "too many backups to list here; connect the disk to a network storage and search it there"
					f.Snapshots = nil
					break
				}
				f.Snapshots = append(f.Snapshots, api.FoundSnapshot{ID: sn.ID.String(), Snapshot: b})
			}
			snaps += len(f.Snapshots)
		}
		switch {
		case errors.Is(err, repo.ErrWrongPassword), errors.Is(err, repo.ErrPasswordRequired):
			f.Error = "encrypted with another recovery key"
		case err != nil:
			f.Error = err.Error()
		}
		found = append(found, f)
	}
	res.Status = api.StatusSuccess
	res.Details, _ = json.Marshal(api.RepoScan{Base: base, Repos: found})
	res.Message = fmt.Sprintf("Found %d repositories with %d backups", len(found), snaps)
	return res
}

// joinLocation is the location of folder dir in a storage location (a
// local path or a URL).
func joinLocation(base, dir string) string {
	if strings.Contains(base, "://") {
		if u, err := url.Parse(base); err == nil {
			u.Path = path.Join(u.Path, dir)
			return u.String()
		}
	}
	return strings.TrimRight(base, `/\`) + "/" + dir
}
