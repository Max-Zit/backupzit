package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/restorer"
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
	if len(st.Errors) > 0 {
		res.Status = api.StatusFailed
		res.Message = fmt.Sprintf("RESTORE TEST FAILED: %d problems, e.g. %s", len(st.Errors), strings.SplitN(st.Errors[0], "\n", 2)[0])
	}
	return res
}
