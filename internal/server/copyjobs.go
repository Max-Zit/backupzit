package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
)

// queueCopy creates a copy run for a copy job: the agent of the source job
// copies its backups from the source job's repository into this job's
// target.
func (s *Store) queueCopy(ctx context.Context, j Job, a Agent, dst Target, trigger string) (int64, error) {
	if j.SourceJobID == nil {
		return 0, errors.New("copy job without source job")
	}
	src, err := s.GetJob(ctx, *j.SourceJobID)
	if err != nil {
		return 0, errors.New("the source job was deleted")
	}
	st, err := s.GetTarget(ctx, src.TargetID)
	if err != nil {
		return 0, err
	}
	// The copy keeps the repository folder of the source job.
	dir, err := s.jobRepoDir(ctx, src, a)
	if err != nil {
		return 0, err
	}
	da := Agent{RepoDir: dir}
	if parts := s.copyParts(ctx, src); parts != nil {
		return s.queueCopyParts(ctx, j, trigger, repoURL(dst, da), dst.ID, st.ID, repoURL(st, da), parts)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockJobRow(ctx, tx, j.ID); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, source_target_id, source_repo_url)
		SELECT $1,$2,'copy',$3,$4,$5,$6,$7
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE job_id=$2 AND kind='copy' AND status IN ('queued','running'))
		RETURNING id`,
		j.AgentID, j.ID, trigger, repoURL(dst, da), dst.ID, st.ID, repoURL(st, da)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRunActive
	}
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// lockJobRow locks the job's row until the transaction ends (A16): the
// "no run queued or running yet" check and the insert of a new run are
// then one step, so two starts at the same moment (schedule and button,
// a chain and a copy after the backup) cannot both queue a run. Not a
// unique index: split VMware runs queue several runs on purpose.
func lockJobRow(ctx context.Context, tx pgx.Tx, jobID int64) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM jobs WHERE id=$1 FOR UPDATE`, jobID)
	return err
}

// QueueCopiesAfter queues the copy jobs that run after each backup of
// jobID ("after" schedule). Not while the job's retention is paused after
// a suspicious backup: a ransomware run must not reach the off-site copy
// before someone has checked the machine (copy jobs with their own
// schedule still copy, but remove nothing, see addRetention).
func (s *Store) QueueCopiesAfter(ctx context.Context, jobID int64) ([]int64, error) {
	var hold bool
	if err := s.db.QueryRow(ctx, `SELECT retention_hold FROM jobs WHERE id=$1`, jobID).Scan(&hold); err != nil {
		return nil, err
	}
	if hold {
		slog.Info("copy jobs not started: the job's retention is paused after a suspicious backup", "job", jobID)
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM jobs WHERE source_job_id=$1 AND kind='copy' AND enabled
		AND schedule LIKE '%"kind":"after"%'`, jobID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	var queued []int64
	for _, id := range ids {
		rid, err := s.QueueBackup(ctx, id, "after-backup")
		if errors.Is(err, ErrRunActive) {
			continue
		}
		if err != nil {
			return queued, err
		}
		queued = append(queued, rid)
	}
	return queued, nil
}

// addCopySource fills the source repository of a copy run.
func (s *Server) addCopySource(ctx context.Context, run *Run, ar *api.Run) error {
	if run.SourceTargetID == nil || run.JobID == nil {
		return errors.New("source storage target was deleted")
	}
	j, err := s.store.GetJob(ctx, *run.JobID)
	if err != nil || j.SourceJobID == nil {
		return errors.New("copy job was deleted")
	}
	t, err := s.store.GetTarget(ctx, *run.SourceTargetID)
	if err != nil {
		return errors.New("source storage target was deleted")
	}
	src := targetRepository(t, run.SourceRepoURL)
	ar.Source = &src
	ar.SourceTag = fmt.Sprintf("job:%d", *j.SourceJobID)
	return nil
}

// targetRepository describes a repository on a storage target for agents.
func targetRepository(t Target, url string) api.Repository {
	return api.Repository{
		URL:                 url,
		SFTPPassword:        t.SFTPPassword,
		SFTPKey:             t.SFTPKey,
		SFTPHostKey:         t.SFTPHostKey,
		S3AccessKey:         t.S3AccessKey,
		S3SecretKey:         t.S3SecretKey,
		S3Region:            t.S3Region,
		S3LockDays:          t.S3LockDays,
		SMBPassword:         t.SMBPassword,
		SMBDomain:           t.SMBDomain,
		HardenedKey:         t.HardenedKey,
		HardenedFingerprint: t.HardenedFingerprint,
		AzureKey:            t.AzureKey,
		AzureSAS:            t.AzureSAS,
		Password:            t.RecoveryKey,
	}
}

// sourceJobs lists jobs whose backups can be copied.
func sourceJobs(jobs []Job) []Job {
	var out []Job
	for _, j := range jobs {
		if j.Kind != JobCopy {
			out = append(out, j)
		}
	}
	return out
}

// CopyRunStats is the statistics of a copy run for the run page.
type CopyRunStats struct {
	Snapshots   int    `json:"snapshots"`
	Skipped     int    `json:"snapshots_skipped"`
	Blobs       int    `json:"blobs"`
	BytesRead   uint64 `json:"bytes_read"`
	BytesStored uint64 `json:"bytes_stored"`
	// 0.62 (#75): see repo.CopyStats.
	First        bool    `json:"first"`
	Packed       uint64  `json:"packed"`
	ReadSeconds  float64 `json:"read_seconds"`
	WriteSeconds float64 `json:"write_seconds"`
}

// ReadSpeed is how fast the source gave its data while it was read, ""
// when not known (agents before 0.62, or too short to tell).
func (c *CopyRunStats) ReadSpeed() string { return speed(c.Packed, c.ReadSeconds) }

// WriteSpeed is how fast the destination took the uploads.
func (c *CopyRunStats) WriteSpeed() string { return speed(c.BytesStored, c.WriteSeconds) }

func speed(bytes uint64, secs float64) string {
	if secs < 1 || bytes == 0 {
		return ""
	}
	return humanBytes(uint64(float64(bytes)/secs)) + "/s"
}

// copyOfFiles reports whether a copy run copied file backups (not images).
func (s *Store) copyOfFiles(ctx context.Context, run Run) bool {
	if run.Kind != api.KindCopy || run.JobID == nil {
		return false
	}
	j, err := s.GetJob(ctx, *run.JobID)
	if err != nil || j.SourceJobID == nil {
		return false
	}
	src, err := s.GetJob(ctx, *j.SourceJobID)
	return err == nil && (src.Kind == JobFiles || src.Kind == JobNAS || src.Kind == JobM365)
}

// recordUSBDisk stores which rotating disk a backup went to, so restores
// ask for that disk. The agent may only narrow a usb:// pattern to a disk
// with the same path, never point the run elsewhere.
func (s *Store) recordUSBDisk(ctx context.Context, runID int64, actual string) {
	r, err := s.GetRun(ctx, runID)
	if err != nil || !strings.HasPrefix(r.RepoURL, "usb://") || !strings.HasPrefix(actual, "usb://") {
		return
	}
	want, err1 := url.Parse(r.RepoURL)
	got, err2 := url.Parse(actual)
	if err1 != nil || err2 != nil || want.Path != got.Path || got.Host == "" {
		return
	}
	if ok, _ := path.Match(strings.ToUpper(want.Host), strings.ToUpper(got.Host)); !ok {
		return
	}
	_, _ = s.db.Exec(ctx, `UPDATE runs SET repo_url=$2 WHERE id=$1`, runID, got.String())
}
