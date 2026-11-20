package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
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
		a.progress(ctx, uint64(done), uint64(total), 0) // #nosec G115 -- counts and sizes for display, not negative
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

// repoRepair checks the repository again under the exclusive lock and takes
// the damaged packs out of the index (KindRepoRepair): later backups then
// store their data again instead of reusing it. The backups that need the
// lost data are listed; they stay and restore what is left, unless the run
// asks to remove them (RepairForget; never the ones in KeepSnapshots).
func (a *Agent) repoRepair(ctx context.Context, run api.Run) api.RunResult {
	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	lock, err := r.Lock(ctx, true, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	res, err := checker.Run(ctx, r, checker.Options{ReadData: true, Progress: func(done, total int) {
		a.progress(ctx, uint64(done), uint64(total), 0) // #nosec G115 -- counts for display
	}})
	if err != nil {
		return failed(fmt.Errorf("check: %w", err))
	}
	// Packs this agent cannot read while the storage holds them may only be
	// cut short on the way here: they are never taken out (#38).
	unreadable := ""
	if n := len(res.Unreadable); n > 0 {
		unreadable = fmt.Sprintf(". %s could not be read from this agent and %s left alone: the storage may be fine, check the network path (proxy, firewall, IPS, NAT)",
			count(n, "data pack", "data packs"), map[bool]string{true: "was", false: "were"}[n == 1])
	}
	if len(res.Damaged) == 0 {
		if unreadable != "" {
			return api.RunResult{Status: api.StatusWarning, Errors: res.Errors,
				Message: fmt.Sprintf("No data pack is proven damaged, nothing was taken out%s", unreadable)}
		}
		return api.RunResult{Status: api.StatusSuccess, Message: fmt.Sprintf("Read all %d data packs again: none is damaged now, nothing to repair", res.PacksRead)}
	}
	rep, err := r.DropDamagedPacks(ctx, res.Damaged)
	if err != nil {
		return failed(fmt.Errorf("repair: %w", err))
	}
	out := api.RunResult{Status: api.StatusWarning}
	out.Message = fmt.Sprintf("Took %d damaged data packs (%d blocks) out of the index: the next backups store that data again. %d backups need the lost data and cannot be restored completely",
		rep.Packs, rep.Blobs, len(rep.Snapshots))
	// Optionally the affected backups go too: as long as they exist, every
	// later data check reports the missing data. Immutable storage may keep
	// them until their lock expires; those are listed as errors.
	notRemoved := 0
	describe := func(sn *repo.Snapshot) string {
		return fmt.Sprintf("backup %s of %s from %s", sn.ID.Short(), sn.Hostname, sn.Time.Local().Format("2006-01-02 15:04"))
	}
	if run.RepairForget {
		keep := map[string]bool{}
		for _, id := range run.KeepSnapshots {
			keep[id] = true
		}
		for _, sn := range rep.Snapshots {
			if keep[sn.ID.String()] {
				notRemoved++
				if len(out.Errors) < maxCheckErrors {
					out.Errors = append(out.Errors, describe(sn)+" needs lost data and was not removed: a VM runs from it (instant recovery)")
				}
				continue
			}
			if err := r.RemoveSnapshot(ctx, sn.ID); err != nil {
				notRemoved++
				if len(out.Errors) < maxCheckErrors {
					out.Errors = append(out.Errors, fmt.Sprintf("%s needs lost data and was not removed: %v", describe(sn), err))
				}
				continue
			}
			out.Forgotten = append(out.Forgotten, sn.ID.String())
		}
		out.Message = fmt.Sprintf("Took %d damaged data packs (%d blocks) out of the index: the next backups store that data again. Removed %d of the %d backups that needed the lost data",
			rep.Packs, rep.Blobs, len(out.Forgotten), len(rep.Snapshots))
	} else {
		for i, sn := range rep.Snapshots {
			if i == maxCheckErrors {
				out.Errors = append(out.Errors, fmt.Sprintf("... and %d more", len(rep.Snapshots)-maxCheckErrors))
				break
			}
			out.Errors = append(out.Errors, describe(sn)+" needs lost data")
		}
	}
	out.Message += unreadable
	stats, _ := json.Marshal(map[string]int{"packs": rep.Packs, "blobs": rep.Blobs, "unreadable": len(res.Unreadable),
		"backups_affected": len(rep.Snapshots), "backups_removed": len(out.Forgotten), "backups_not_removed": notRemoved})
	out.Stats = stats
	return out
}
