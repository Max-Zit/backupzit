package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/m365"
	"github.com/max-zit/backupzit/internal/repo"
)

// m365Backup reads Microsoft 365 mailboxes and OneDrives through
// Microsoft Graph; any agent that reaches the internet can do it. The
// previous backup of the job is the parent: messages and files that did
// not change are not downloaded again.
func (a *Agent) m365Backup(ctx context.Context, run api.Run) api.RunResult {
	src := run.M365
	r, closeRepo, err := a.openRepo(ctx, run.Repository, true)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	parent, err := lastJobSnapshot(ctx, r, run.JobID)
	if err != nil {
		return failed(err)
	}
	c := m365.NewClient(m365.Credentials{Tenant: src.Tenant, Client: src.Client, Secret: src.Secret})
	res, err := m365.Backup(ctx, r, c, m365.Options{
		Users:      run.Paths,
		Mail:       src.Mail,
		OneDrive:   src.OneDrive,
		Calendar:   src.Calendar,
		SharePoint: src.SharePoint,
		Sites:      src.Sites,
		Parent:     parent,
		Hostname:   "m365-" + src.Tenant,
		Version:    a.version,
		Tags:       []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)},
		Progress:   func(s *repo.SnapshotStats) { a.progress(s.BytesRead, 0, s.Files) },
	})
	if err != nil {
		return failed(fmt.Errorf("Microsoft 365: %w", err))
	}
	sn := res.Snapshot
	stats, _ := json.Marshal(sn.Stats)
	out := api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Errors: sn.Stats.Errors}
	var parts []string
	if len(res.Accounts) > 0 {
		parts = append(parts, count(len(res.Accounts), "account", "accounts")+": "+strings.Join(res.Accounts, ", "))
	}
	if len(res.Sites) > 0 {
		parts = append(parts, count(len(res.Sites), "SharePoint site", "SharePoint sites")+": "+strings.Join(res.Sites, ", "))
	}
	out.Message = strings.Join(parts, ". ")
	if len(res.Skipped) > 0 {
		out.Message += fmt.Sprintf(". %s without mailbox or OneDrive skipped", count(len(res.Skipped), "account", "accounts"))
	}
	if n, wait := c.Throttled(); n > 0 {
		out.Message += fmt.Sprintf(". Microsoft 365 asked %d times to slow down (waited %s)", n, wait.Round(time.Second))
	}
	if len(sn.Stats.Errors) > 0 {
		out.Status = api.StatusWarning
		out.Message = fmt.Sprintf("%d messages, files or folders could not be read. %s", len(sn.Stats.Errors), out.Message)
	}
	lock.Unlock() // retention needs an exclusive lock
	return a.withRetention(ctx, r, run, out)
}

// lastJobSnapshot is the newest snapshot of a job, or nil.
func lastJobSnapshot(ctx context.Context, r *repo.Repository, jobID int64) (*repo.Snapshot, error) {
	if jobID == 0 {
		return nil, nil
	}
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	var last *repo.Snapshot
	for _, sn := range sns {
		if hasTag(sn, jobTag(jobID)) && (last == nil || sn.Time.After(last.Time)) {
			last = sn
		}
	}
	return last, nil
}
