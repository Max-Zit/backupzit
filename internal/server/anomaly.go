package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/anomaly"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
)

// Ransomware and mass-deletion detection: every finished backup is compared
// with the job's recent history. Encrypting or deleting many files leaves
// clear traces — far more changed files than usual, new data that no longer
// compresses (encrypted data is random), a sudden drop of the file count or
// a jump of the new data volume. A suspicious backup is flagged, alerts are
// sent and the job's retention is paused so that older, clean backups are
// not removed in favor of the damaged ones.

// detectAnomaly returns human-readable reasons why cur looks suspicious
// compared with hist (older backups of the same job, newest first); the
// agent runs the same check before retention (anomaly.Detect).
func detectAnomaly(kind string, cur repo.SnapshotStats, hist []repo.SnapshotStats) []string {
	return anomaly.Detect(kind, cur, hist)
}

// checkAnomaly analyses a finished backup run and flags it.
func (s *Store) checkAnomaly(ctx context.Context, runID int64) (string, error) {
	r, err := s.GetRun(ctx, runID)
	if err != nil || r.JobID == nil || len(r.Stats) == 0 || r.Status == api.StatusFailed ||
		(r.Kind != api.KindBackup && r.Kind != api.KindImageBackup && r.Kind != api.KindSystemBackup && r.Kind != api.KindVMBackup) {
		return "", err
	}
	var cur repo.SnapshotStats
	if json.Unmarshal(r.Stats, &cur) != nil {
		return "", nil
	}
	hist, err := s.recentStats(ctx, *r.JobID, r.ID, r.Kind, r.Part)
	if err != nil {
		return "", err
	}
	reasons := detectAnomaly(r.Kind, cur, hist)
	if len(reasons) == 0 {
		return "", nil
	}
	text := strings.Join(reasons, "; ")
	if _, err := s.db.Exec(ctx, `UPDATE runs SET anomaly=$2 WHERE id=$1`, r.ID, text); err != nil {
		return "", err
	}
	if _, err := s.db.Exec(ctx, `UPDATE jobs SET retention_hold=true WHERE id=$1`, *r.JobID); err != nil {
		return "", err
	}
	return text, nil
}

// releaseRetentionHold resumes retention of a job and marks its
// suspicious backups as reviewed.
func (s *Store) releaseRetentionHold(ctx context.Context, id int64) error {
	if _, err := s.db.Exec(ctx, `UPDATE jobs SET retention_hold=false WHERE id=$1`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE runs SET anomaly_ack=true WHERE job_id=$1 AND anomaly<>''`, id)
	return err
}

func (s *Server) handleJobReleaseHold(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.needsApproval(w, r, fmt.Sprintf("/jobs/%d", id), "job.release_hold", id, j.Name, nil) {
		return
	}
	if err := s.store.releaseRetentionHold(r.Context(), id); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "job.anomaly_ack", "job %d: suspicious backups reviewed, retention resumed", id)
	redirectMsg(w, r, fmt.Sprintf("/jobs/%d", id), "Retention resumed.")
}

// recentStats are the statistics of the job's last ten clean backups of
// kind (newest first), without the run except. A part of a split VMware
// backup is compared with the same part only (the proxies back up
// different VMs).
func (s *Store) recentStats(ctx context.Context, jobID, except int64, kind, part string) ([]repo.SnapshotStats, error) {
	rows, err := s.db.Query(ctx, `SELECT stats FROM runs WHERE job_id=$1 AND id<>$2 AND kind=$3 AND part=$4
		AND status IN ('success','warning') AND stats IS NOT NULL AND anomaly='' ORDER BY finished_at DESC LIMIT 10`, jobID, except, kind, part)
	if err != nil {
		return nil, err
	}
	raws, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	var hist []repo.SnapshotStats
	for _, b := range raws {
		var h repo.SnapshotStats
		if json.Unmarshal(b, &h) == nil {
			hist = append(hist, h)
		}
	}
	return hist, nil
}
