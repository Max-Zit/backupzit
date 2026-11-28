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
		switch d, u := len(res.Damaged), len(res.Unreadable); {
		case d > 0 && u > 0:
			out.Message += fmt.Sprintf(". %s damaged on the storage; %s could not be read from this agent (it may be the network path)", count(d, "pack", "packs"), count(u, "pack", "packs"))
		case d > 0:
			out.Message += fmt.Sprintf(". %s damaged on the storage", count(d, "pack", "packs"))
		case u > 0:
			// Only unreadable from here: say so instead of "damaged" (#38).
			out.Message = fmt.Sprintf("%s could not be read from this agent, although the storage holds them: check the network path (proxy, firewall, IPS, NAT) before anything else", count(u, "data pack", "data packs"))
		}
		if n := len(res.DamagedIndex); n > 0 {
			out.Message += fmt.Sprintf(". %s damaged: Repair writes a new index", count(n, "index file", "index files"))
		}
		if n := len(res.TooLarge); n > 0 {
			// Not read and not counted as damaged (MED 5, 0.63).
			out.Message += fmt.Sprintf(". %s larger than BackupZit ever writes a pack: not read and not counted as damaged (repair leaves them alone; check those files on the storage)", count(n, "data pack is", "data packs are"))
		}
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
	// Damaged index files: their packs were read from the pack headers when
	// the repository was opened; a new index replaces them.
	// Packs it leaves out (proven damaged) are reported like those that
	// DropDamagedPacks takes out: with the backups that need their data.
	rebuilt := ""
	rep := &repo.RepairResult{}
	if bad := r.DamagedIndexFiles(); len(bad) > 0 {
		// With "remove affected backups" the backups that use data no pack
		// holds any more count as affected too (only if the storage lists
		// the same packs twice); without it the rebuild names them and stops.
		rb, err := r.RebuildIndexWith(ctx, repo.RebuildOptions{AcceptLost: run.RepairForget})
		if err != nil {
			return failed(fmt.Errorf("rebuild index: %w", err))
		}
		rebuilt = fmt.Sprintf("Replaced %s by a new index for %d data packs (the damaged files are kept in index-damaged/). ", count(len(bad), "damaged index file", "damaged index files"), rb.Packs)
		if rb.Missing > 0 {
			rebuilt += fmt.Sprintf("%s that backups use %s in no pack at all: those backups count as affected. ", count(rb.Missing, "blob", "blobs"), map[bool]string{true: "was", false: "were"}[rb.Missing == 1])
		}
		rep = rb.Repair()
	}
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
	if len(res.Damaged) == 0 && rep.Packs == 0 {
		if unreadable != "" {
			return api.RunResult{Status: api.StatusWarning, Errors: res.Errors,
				Message: rebuilt + fmt.Sprintf("No data pack is proven damaged, nothing was taken out%s", unreadable)}
		}
		if len(res.Errors) > 0 { // other problems the check found stay visible (L-N7)
			return api.RunResult{Status: api.StatusWarning, Errors: res.Errors,
				Message: rebuilt + fmt.Sprintf("No data pack is proven damaged, nothing was taken out; the check found %s", count(len(res.Errors), "other problem", "other problems"))}
		}
		return api.RunResult{Status: api.StatusSuccess, Message: rebuilt + fmt.Sprintf("Read all %d data packs again: none is damaged now, nothing to repair", res.PacksRead)}
	}
	if len(res.Damaged) > 0 {
		dropped, err := r.DropDamagedPacks(ctx, res.Damaged)
		if err != nil {
			return failed(fmt.Errorf("repair: %w", err))
		}
		rep.Merge(dropped)
	}
	out := api.RunResult{Status: api.StatusWarning}
	out.Message = rebuilt + fmt.Sprintf("Took %d damaged data packs (%d blocks) out of the index: the next backups store that data again. %d backups need the lost data and cannot be restored completely",
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
		out.Message = rebuilt + fmt.Sprintf("Took %d damaged data packs (%d blocks) out of the index: the next backups store that data again. Removed %d of the %d backups that needed the lost data",
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
	if n := len(rep.Unchecked); n > 0 {
		// Their folder lists are in packs this agent cannot read now: nothing
		// proves they need the lost data, so they are kept.
		out.Message += fmt.Sprintf(". %s could not be read completely from this agent and %s kept", count(n, "backup", "backups"),
			map[bool]string{true: "was", false: "were"}[n == 1])
	}
	out.Message += unreadable
	stats, _ := json.Marshal(map[string]int{"packs": rep.Packs, "blobs": rep.Blobs, "unreadable": len(res.Unreadable), "backups_unchecked": len(rep.Unchecked),
		"backups_affected": len(rep.Snapshots), "backups_removed": len(out.Forgotten), "backups_not_removed": notRemoved})
	out.Stats = stats
	return out
}
