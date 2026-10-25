package server

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/logfile"
)

// Diagnostics: Settings → Diagnostics shows where the logs are and offers a
// zip for support with the server log, versions, agents, jobs, recent
// problems and the audit log — without passwords, keys or storage
// credentials.

// diagLogMax limits the server log in the diagnostics zip.
const diagLogMax = 20 << 20

func (s *Server) diagnosticsPage(ctx context.Context) map[string]any {
	d := map[string]any{"LogFile": s.LogFile}
	if s.LogFile != "" {
		if b, err := logfile.Tail(s.LogFile, 0, 64<<10); err == nil {
			d["LogTail"] = logTail(string(b), 100)
		}
	}
	return d
}

// handleDiagnosticsBundle serves GET /settings/diagnostics/bundle.
func (s *Server) handleDiagnosticsBundle(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()
	now := time.Now()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="backupzit-diagnostics-`+now.Format("20060102-1504")+`.zip"`)
	z := zip.NewWriter(w)
	defer z.Close()
	add := func(name string, write func(io.Writer) error) {
		f, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
		if err == nil {
			if err := write(f); err != nil {
				fmt.Fprintf(f, "\n(error: %v)\n", err)
			}
		}
	}
	add("info.txt", func(w io.Writer) error { return s.diagInfo(ctx, w, now) })
	if s.LogFile != "" {
		add("server.log", func(w io.Writer) error {
			b, err := logfile.Tail(s.LogFile, logfile.DefaultKeep, diagLogMax)
			w.Write(b)
			return err
		})
	}
	add("agents.csv", func(w io.Writer) error { return s.diagAgents(ctx, w) })
	add("jobs.csv", func(w io.Writer) error { return s.diagJobs(ctx, w) })
	add("problems.csv", func(w io.Writer) error { return s.diagProblems(ctx, w) })
	add("audit.csv", func(w io.Writer) error { return s.diagAudit(ctx, w) })
	s.audit(r, "diagnostics.download", "diagnostics zip downloaded")
}

func (s *Server) diagInfo(ctx context.Context, w io.Writer, now time.Time) error {
	host, _ := os.Hostname()
	var pg, dbSize string
	s.store.db.QueryRow(ctx, `SELECT version(), pg_size_pretty(pg_database_size(current_database()))`).Scan(&pg, &dbSize)
	count := func(q string) int {
		var n int
		s.store.db.QueryRow(ctx, q).Scan(&n)
		return n
	}
	var upd updateCheck
	s.store.GetSetting(ctx, settingUpdateCheck, &upd)
	fmt.Fprintf(w, "BackupZit console %s\nCreated: %s\nHost: %s (%s/%s, %s)\nPublic URL: %s\nTime zone: %s\nDatabase: %s, %s\nLog file: %s\n\n",
		s.Version, now.Format(time.RFC3339), host, runtime.GOOS, runtime.GOARCH, runtime.Version(), s.PublicURL, now.Format("MST -07:00"), pg, dbSize, s.LogFile)
	fmt.Fprintf(w, "Agents: %d\nJobs: %d (enabled %d)\nStorage targets: %d\nRuns in the last 24 hours: %d (failed %d)\nRunning or queued: %d\n\n",
		count(`SELECT count(*) FROM agents WHERE removed_at IS NULL`), count(`SELECT count(*) FROM jobs`), count(`SELECT count(*) FROM jobs WHERE enabled`),
		count(`SELECT count(*) FROM storage_targets`), count(`SELECT count(*) FROM runs WHERE queued_at > now() - interval '24 hours'`),
		count(`SELECT count(*) FROM runs WHERE queued_at > now() - interval '24 hours' AND status='failed'`),
		count(`SELECT count(*) FROM runs WHERE status IN ('queued','running')`))
	fmt.Fprintf(w, "Last update check: %s %s\n", upd.Time.Format(time.RFC3339), upd.Err)
	return nil
}

func (s *Server) diagAgents(ctx context.Context, w io.Writer) error {
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return err
	}
	c := csv.NewWriter(w)
	c.Write([]string{"id", "hostname", "os", "arch", "version", "online", "last_seen", "enrolled", "recovery", "local_ips"})
	for _, a := range agents {
		seen := ""
		if a.LastSeen != nil {
			seen = a.LastSeen.Format(time.RFC3339)
		}
		c.Write([]string{fmt.Sprint(a.ID), a.Hostname, a.OS, a.Arch, a.Version, fmt.Sprint(a.Online()), seen, a.EnrolledAt.Format(time.RFC3339),
			fmt.Sprint(a.Recovery), strings.Join(a.LocalIPs, " ")})
	}
	c.Flush()
	return c.Error()
}

func (s *Server) diagJobs(ctx context.Context, w io.Writer) error {
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return err
	}
	targets := map[int64]string{}
	if ts, err := s.store.ListTargets(ctx); err == nil {
		for _, t := range ts {
			targets[t.ID] = t.Kind
		}
	}
	c := csv.NewWriter(w)
	c.Write([]string{"id", "name", "kind", "agent", "target", "target_kind", "enabled", "schedule", "time_limit_h", "speed_limit_mbps", "retention_hold"})
	for _, j := range jobs {
		c.Write([]string{fmt.Sprint(j.ID), j.Name, j.Kind, j.Hostname, j.TargetName, targets[j.TargetID], fmt.Sprint(j.Enabled), j.Schedule,
			fmt.Sprint(j.Options.MaxHours), fmt.Sprint(j.Options.LimitMBps), fmt.Sprint(j.RetentionHold)})
	}
	c.Flush()
	return c.Error()
}

// diagProblems lists failed runs and runs with warnings of the last 30
// days.
func (s *Server) diagProblems(ctx context.Context, w io.Writer) error {
	rows, err := s.store.db.Query(ctx, `SELECT r.id, r.kind, r.status, a.hostname, COALESCE(j.name,''), r.queued_at, r.started_at, r.finished_at,
		COALESCE(r.message,''), COALESCE(array_to_string(r.errors[1:5], ' | '),'')
		FROM runs r JOIN agents a ON a.id=r.agent_id LEFT JOIN jobs j ON j.id=r.job_id
		WHERE r.status IN ('failed','warning') AND r.queued_at > now() - interval '30 days'
		ORDER BY r.id DESC LIMIT 1000`)
	if err != nil {
		return err
	}
	defer rows.Close()
	c := csv.NewWriter(w)
	c.Write([]string{"run", "kind", "status", "agent", "job", "queued", "started", "finished", "message", "errors"})
	ts := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	for rows.Next() {
		var id int64
		var kind, status, host, job, msg, errs string
		var queued time.Time
		var started, finished *time.Time
		if err := rows.Scan(&id, &kind, &status, &host, &job, &queued, &started, &finished, &msg, &errs); err != nil {
			return err
		}
		c.Write([]string{fmt.Sprint(id), kind, status, host, job, queued.Format(time.RFC3339), ts(started), ts(finished), msg, errs})
	}
	c.Flush()
	if err := c.Error(); err != nil {
		return err
	}
	return rows.Err()
}

func (s *Server) diagAudit(ctx context.Context, w io.Writer) error {
	entries, err := s.store.ListAudit(ctx, 2000)
	if err != nil {
		return err
	}
	c := csv.NewWriter(w)
	c.Write([]string{"id", "time", "user", "action", "detail", "address"})
	for _, e := range entries {
		c.Write([]string{fmt.Sprint(e.ID), e.At.Format(time.RFC3339), e.Username, e.Action, e.Detail, e.Remote})
	}
	c.Flush()
	return c.Error()
}
