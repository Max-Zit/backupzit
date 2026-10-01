package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/jackc/pgx/v5"
)

const settingRestoreTests = "restore_tests"

// RestoreTestSettings control automatic restore tests: periodically each
// job's newest backup is partly restored and verified by its agent.
type RestoreTestSettings struct {
	Enabled bool   `json:"enabled"`
	Every   string `json:"every"`  // "weekly" | "monthly"
	Files   int    `json:"files"`  // random files per test
	MaxMB   int    `json:"max_mb"` // size limit of the sample
	Blocks  int    `json:"blocks"` // random blocks for disk images
}

var defaultRestoreTests = RestoreTestSettings{Enabled: true, Every: "weekly", Files: 10, MaxMB: 100, Blocks: 100}

func (x RestoreTestSettings) Validate() error {
	if x.Every != "weekly" && x.Every != "monthly" {
		return errors.New("choose weekly or monthly")
	}
	if x.Files < 1 || x.Files > 1000 || x.MaxMB < 1 || x.MaxMB > 100000 || x.Blocks < 1 || x.Blocks > 100000 {
		return errors.New("sample sizes are out of range")
	}
	return nil
}

func (x RestoreTestSettings) interval() time.Duration {
	if x.Every == "monthly" {
		return 30 * 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

func (s *Store) restoreTestSettings(ctx context.Context) RestoreTestSettings {
	x := defaultRestoreTests
	if err := s.GetSetting(ctx, settingRestoreTests, &x); err != nil || x.Validate() != nil {
		return defaultRestoreTests
	}
	return x
}

// QueueRestoreTest creates a restore test of the newest usable backup of
// a job (for copy jobs: of the newest copy).
func (s *Store) QueueRestoreTest(ctx context.Context, jobID int64, trigger string) (int64, error) {
	var b struct {
		agentID, targetID int64
		repoURL, snapshot string
	}
	err := s.db.QueryRow(ctx, `SELECT agent_id, target_id, repo_url, snapshot_id FROM runs
		WHERE job_id=$1 AND kind IN ('backup','image-backup','copy') AND status IN ('success','warning')
		AND snapshot_id<>'' AND NOT expired AND target_id IS NOT NULL
		ORDER BY finished_at DESC LIMIT 1`, jobID).Scan(&b.agentID, &b.targetID, &b.repoURL, &b.snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("the job has no backup to test yet")
	}
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id)
		SELECT $1,$2,'verify',$3,$4,$5,$6
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE job_id=$2 AND kind='verify' AND status IN ('queued','running'))
		RETURNING id`, b.agentID, jobID, trigger, b.repoURL, b.targetID, b.snapshot).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRunActive
	}
	return id, err
}

// queueDueRestoreTests queues tests for jobs not tested within the interval.
func (s *Store) queueDueRestoreTests(ctx context.Context, now time.Time) ([]int64, error) {
	x := s.restoreTestSettings(ctx)
	if !x.Enabled {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `SELECT j.id FROM jobs j WHERE j.enabled
		AND EXISTS (SELECT 1 FROM runs b WHERE b.job_id=j.id AND b.kind IN ('backup','image-backup','copy')
			AND b.status IN ('success','warning') AND b.snapshot_id<>'' AND NOT b.expired)
		AND NOT EXISTS (SELECT 1 FROM runs v WHERE v.job_id=j.id AND v.kind='verify' AND v.queued_at > $1)`,
		now.Add(-x.interval()))
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	var queued []int64
	for _, id := range ids {
		rid, err := s.QueueRestoreTest(ctx, id, "schedule")
		if err != nil {
			continue
		}
		queued = append(queued, rid)
	}
	return queued, nil
}

func (s *Server) handleJobTest(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	back := fmt.Sprintf("/jobs/%d", id)
	rid, err := s.store.QueueRestoreTest(r.Context(), id, "manual")
	if errors.Is(err, ErrRunActive) {
		redirectErr(w, r, back, errors.New("a restore test of this job is already queued"))
		return
	}
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Restore test queued.")
}

func (s *Server) handleSettingsRestoreTests(w http.ResponseWriter, r *http.Request, _ string) {
	atoi := func(k string) int { n, _ := strconv.Atoi(r.FormValue(k)); return n }
	x := RestoreTestSettings{Enabled: r.FormValue("enabled") == "on", Every: r.FormValue("every"),
		Files: atoi("files"), MaxMB: atoi("max_mb"), Blocks: atoi("blocks")}
	if err := x.Validate(); err != nil {
		redirectErr(w, r, "/settings/tests", err)
		return
	}
	if err := s.store.SetSetting(r.Context(), settingRestoreTests, x); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "settings.restore_tests", "enabled=%v every=%s files=%d", x.Enabled, x.Every, x.Files)
	redirectMsg(w, r, "/settings/tests", "Restore test settings saved.")
}

// addVerifyParams sets the sample size of a restore test run.
func (s *Server) addVerifyParams(ctx context.Context, ar *api.Run) {
	x := s.store.restoreTestSettings(ctx)
	ar.VerifyFiles, ar.VerifyMaxBytes, ar.VerifyBlocks = x.Files, uint64(x.MaxMB)<<20, x.Blocks
}
