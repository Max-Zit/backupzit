package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/checker"
)

// maxCheckErrors limits the problems a repository check reports.
const maxCheckErrors = 50

// repoCheck reads every pack of the repository and verifies every blob, and
// checks that each backup's trees and data are present (KindRepoCheck).
func (a *Agent) repoCheck(ctx context.Context, run api.Run) api.RunResult {
	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	res, err := checker.Run(ctx, r, checker.Options{ReadData: true, Progress: func(done, total int) {
		a.progress(ctx, uint64(done), uint64(total), 0)
	}})
	if err != nil {
		return failed(fmt.Errorf("check: %w", err))
	}
	stats, _ := json.Marshal(res)
	out := api.RunResult{Status: api.StatusSuccess, Stats: stats}
	if !res.OK() {
		out.Status = api.StatusFailed
		out.Errors = res.Errors
		if len(out.Errors) > maxCheckErrors {
			out.Errors = append(out.Errors[:maxCheckErrors], fmt.Sprintf("... and %d more", len(res.Errors)-maxCheckErrors))
		}
		out.Message = fmt.Sprintf("%d problems found in %d backups and %d data packs: some backups may not be restorable", len(res.Errors), res.Snapshots, res.Packs)
		return out
	}
	out.Message = fmt.Sprintf("Checked %d backups and read all %d data packs (%d blocks): no errors", res.Snapshots, res.PacksRead, res.BlobsRead)
	if len(res.Warnings) > 0 {
		out.Message += fmt.Sprintf("; %d unused data packs left over from interrupted backups (harmless)", len(res.Warnings))
	}
	return out
}
