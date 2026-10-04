package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// verifyRun is a restore test: it restores a random sample of a backup
// into a temporary folder (or reads random image blocks) and verifies it.
func (a *Agent) verifyRun(ctx context.Context, run api.Run) api.RunResult {
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
	sn, err := r.LoadSnapshot(ctx, run.SnapshotID)
	if err != nil {
		return failed(fmt.Errorf("load backup: %w", err))
	}
	st, err := restorer.SampleVerify(ctx, r, sn, restorer.SampleOptions{
		Files: run.VerifyFiles, MaxBytes: run.VerifyMaxBytes, Blocks: run.VerifyBlocks, TempDir: os.TempDir(),
	})
	if err != nil {
		return failed(fmt.Errorf("restore test: %w", err))
	}
	stats, _ := json.Marshal(st)
	res := api.RunResult{Status: api.StatusSuccess, Stats: stats, Errors: st.Errors}
	switch {
	case len(sn.Images) > 0:
		res.Message = fmt.Sprintf("Read and verified %d random blocks (%s) of %d", st.Blocks, humanSize(st.Bytes), st.Candidates)
	case st.Candidates == 0:
		res.Message = "The backup contains no files"
	default:
		res.Message = fmt.Sprintf("Restored and verified %d random files (%s) of %d", st.Files, humanSize(st.Bytes), st.Candidates)
	}
	if run.VerifyBootSeconds > 0 && len(st.Errors) == 0 {
		// The shared lock stays held: no prune while VMs read the backup.
		a.bootTests(ctx, run, sn, &res)
		if res.Status == api.StatusFailed {
			return res
		}
	}
	if len(st.Errors) > 0 {
		res.Status = api.StatusFailed
		res.Message = fmt.Sprintf("RESTORE TEST FAILED: %d problems, e.g. %s", len(st.Errors), strings.SplitN(st.Errors[0], "\n", 2)[0])
	}
	return res
}

// bootTests starts each Proxmox VM of a VM backup isolated from the network
// and checks that it boots (restore test with boot test).
func (a *Agent) bootTests(ctx context.Context, run api.Run, sn *repo.Snapshot, res *api.RunResult) {
	var vms []int
	for _, g := range sn.Guests {
		if g.Type == "qemu" && (g.Platform == "" || g.Platform == "proxmox") {
			vms = append(vms, g.VMID)
		}
	}
	if len(vms) == 0 || !pve.Available() {
		return
	}
	var results []pve.BootResult
	booted, running := 0, 0
	var failures []string
	for _, vmid := range vms {
		a.log.Info("boot test", "run", run.ID, "vmid", vmid)
		br := pve.BootTest(ctx, sn, vmid, time.Duration(run.VerifyBootSeconds)*time.Second, pve.InstantOptions{
			ServeCommand: func(ctx context.Context, target int, socket string) error {
				return StartInstantServer(ctx, run.Repository, sn.ID.String(), vmid, target, socket)
			},
			Log: func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) },
		})
		results = append(results, br)
		switch {
		case br.Booted:
			booted++
		case br.Running:
			running++
		default:
			failures = append(failures, fmt.Sprintf("VM %d (%s): %s", br.VMID, br.Name, br.Error))
		}
	}
	details, _ := json.Marshal(map[string]any{"boot": results})
	res.Details = details
	msg := fmt.Sprintf("; boot test: %d of %d VMs booted", booted, len(vms))
	if running > 0 {
		msg += fmt.Sprintf(" (%d without QEMU guest agent kept running)", running)
	}
	res.Message += msg
	if len(failures) > 0 {
		res.Status = api.StatusFailed
		res.Errors = append(res.Errors, failures...)
		res.Message = "BOOT TEST FAILED: " + strings.Join(failures, "; ") + ". " + res.Message
	}
}
