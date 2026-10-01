package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/jackc/pgx/v5"
)

// Ransomware and mass-deletion detection: every finished backup is compared
// with the job's recent history. Encrypting or deleting many files leaves
// clear traces — far more changed files than usual, new data that no longer
// compresses (encrypted data is random), a sudden drop of the file count or
// a jump of the new data volume. A suspicious backup is flagged, alerts are
// sent and the job's retention is paused so that older, clean backups are
// not removed in favor of the damaged ones.

const minHistory = 3

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64{}, v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// detectAnomaly returns human-readable reasons why cur looks suspicious
// compared with hist (older backups of the same job, newest first).
func detectAnomaly(kind string, cur repo.SnapshotStats, hist []repo.SnapshotStats) []string {
	if len(hist) < minHistory {
		return nil
	}
	var reasons []string
	const mb = 1 << 20
	if kind == api.KindBackup {
		changed := cur.FilesNew + cur.FilesChanged
		ratio := float64(changed) / float64(max(cur.Files, 1))
		var base []float64
		for _, h := range hist {
			base = append(base, float64(h.FilesNew+h.FilesChanged)/float64(max(h.Files, 1)))
		}
		if b := median(base); changed >= 100 && ratio >= 0.3 && ratio >= 5*b {
			reasons = append(reasons, fmt.Sprintf("%d files (%.0f %%) changed or new — usually %.1f %%", changed, 100*ratio, 100*b))
		}
		if prev := hist[0].Files; prev >= 100 && cur.Files < prev && float64(prev-cur.Files)/float64(prev) >= 0.3 {
			reasons = append(reasons, fmt.Sprintf("the number of files dropped from %d to %d", prev, cur.Files))
		}
	}
	if cur.BytesAdded >= 50*mb {
		var comp []float64
		for _, h := range hist {
			if h.BytesAdded >= 10*mb {
				comp = append(comp, float64(h.BytesStored)/float64(h.BytesAdded))
			}
		}
		c := float64(cur.BytesStored) / float64(cur.BytesAdded)
		if b := median(comp); len(comp) >= 2 && b <= 0.85 && c >= 0.97 {
			reasons = append(reasons, fmt.Sprintf("new data does not compress (%.0f %% vs. usually %.0f %%) — typical for encrypted files", 100*c, 100*b))
		}
	}
	var added []float64
	for _, h := range hist {
		added = append(added, float64(h.BytesAdded))
	}
	if b := median(added); cur.BytesAdded >= 1<<30 && float64(cur.BytesAdded) >= 10*b {
		reasons = append(reasons, fmt.Sprintf("%s of new data — usually %s", humanBytes(cur.BytesAdded), humanBytes(uint64(b))))
	}
	return reasons
}

// checkAnomaly analyses a finished backup run and flags it.
func (s *Store) checkAnomaly(ctx context.Context, runID int64) (string, error) {
	r, err := s.GetRun(ctx, runID)
	if err != nil || r.JobID == nil || len(r.Stats) == 0 || r.Status == api.StatusFailed ||
		(r.Kind != api.KindBackup && r.Kind != api.KindImageBackup) {
		return "", err
	}
	var cur repo.SnapshotStats
	if json.Unmarshal(r.Stats, &cur) != nil {
		return "", nil
	}
	rows, err := s.db.Query(ctx, `SELECT stats FROM runs WHERE job_id=$1 AND id<>$2 AND kind=$3
		AND status IN ('success','warning') AND stats IS NOT NULL AND anomaly='' ORDER BY finished_at DESC LIMIT 10`, *r.JobID, r.ID, r.Kind)
	if err != nil {
		return "", err
	}
	raws, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return "", err
	}
	var hist []repo.SnapshotStats
	for _, b := range raws {
		var h repo.SnapshotStats
		if json.Unmarshal(b, &h) == nil {
			hist = append(hist, h)
		}
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

func (s *Server) handleJobReleaseHold(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	if _, err := s.store.db.Exec(r.Context(), `UPDATE jobs SET retention_hold=false WHERE id=$1`, id); err != nil {
		s.serverError(w, err)
		return
	}
	s.store.db.Exec(r.Context(), `UPDATE runs SET anomaly_ack=true WHERE job_id=$1 AND anomaly<>''`, id)
	s.audit(r, "job.anomaly_ack", "job %d: suspicious backups reviewed, retention resumed", id)
	redirectMsg(w, r, fmt.Sprintf("/jobs/%d", id), "Retention resumed.")
}
