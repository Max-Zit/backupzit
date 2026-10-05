package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
)

// JobCoverage summarises what a job protects, from its newest successful
// backup, for the job list, the job page and the dashboard.
type JobCoverage struct {
	Unit   string   // "VM", "database" or "" (files, images, systems)
	Items  []string // names of the VMs or databases
	LastOK *time.Time
	Points int    // restore points (backups not removed by retention)
	Size   uint64 // data in the newest backup
}

// Count is the number of protected VMs or databases.
func (c JobCoverage) Count() int { return len(c.Items) }

// Summary is e.g. "3 VMs: web, db, mail" (at most five names).
func (c JobCoverage) Summary() string {
	if c.Unit == "" || len(c.Items) == 0 {
		return ""
	}
	unit := c.Unit
	if len(c.Items) != 1 {
		unit += "s"
	}
	names := c.Items
	more := ""
	if len(names) > 5 {
		names, more = names[:5], fmt.Sprintf(" and %d more", len(c.Items)-5)
	}
	return fmt.Sprintf("%d %s: %s%s", len(c.Items), unit, strings.Join(names, ", "), more)
}

// jobCoverage returns the coverage of every job.
func (s *Store) jobCoverage(ctx context.Context) (map[int64]*JobCoverage, error) {
	out := map[int64]*JobCoverage{}
	get := func(id int64) *JobCoverage {
		c := out[id]
		if c == nil {
			c = &JobCoverage{}
			out[id] = c
		}
		return c
	}
	rows, err := s.db.Query(ctx, `SELECT job_id, count(*) FROM runs
		WHERE job_id IS NOT NULL AND kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup')
		AND status IN ('success','warning') AND coalesce(snapshot_id,'') <> '' AND NOT expired GROUP BY job_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		get(id).Points = n
	}
	rows.Close()
	rows, err = s.db.Query(ctx, `SELECT DISTINCT ON (job_id) job_id, kind, finished_at, details, stats FROM runs
		WHERE job_id IS NOT NULL AND kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log')
		AND status IN ('success','warning') AND finished_at IS NOT NULL ORDER BY job_id, finished_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind string
		var fin time.Time
		var details, stats []byte
		if err := rows.Scan(&id, &kind, &fin, &details, &stats); err != nil {
			return nil, err
		}
		c := get(id)
		c.LastOK = &fin
		var st struct {
			Bytes     uint64 `json:"bytes"`
			BytesRead uint64 `json:"bytes_read"`
		}
		json.Unmarshal(stats, &st)
		c.Size = max(st.Bytes, st.BytesRead)
		var d struct {
			Guests []struct {
				Name string `json:"name"`
			} `json:"guests"`
		}
		json.Unmarshal(details, &d)
		for _, g := range d.Guests {
			c.Unit, c.Items = "VM", append(c.Items, g.Name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// SQL Server: the databases of the newest full backup.
	rows2, err := s.db.Query(ctx, `SELECT DISTINCT ON (job_id) job_id, details FROM runs
		WHERE kind='sql-backup' AND status IN ('success','warning') AND details IS NOT NULL ORDER BY job_id, finished_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var id int64
		var b []byte
		if err := rows2.Scan(&id, &b); err != nil {
			return nil, err
		}
		if d := sqlDetails(Run{Kind: api.KindSQLBackup, Details: b}); d != nil {
			c := get(id)
			c.Unit, c.Items = "database", nil
			for _, db := range d.Databases {
				if !strings.HasPrefix(db.Name, "(") { // (cluster), (globals)
					c.Items = append(c.Items, db.Name)
				}
			}
		}
	}
	return out, rows2.Err()
}

// Attention is something on the dashboard that needs a look.
type Attention struct {
	Severity string // failed | warning
	Title    string
	Detail   string
	Link     string
}

// attention lists failing jobs, offline agents and jobs without a recent
// successful backup.
func (s *Server) attention(ctx context.Context, now time.Time) []Attention {
	var out []Attention
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return nil
	}
	cov, _ := s.store.jobCoverage(ctx)
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		link := fmt.Sprintf("/jobs/%d", j.ID)
		if j.LastStatus != nil && *j.LastStatus == api.StatusFailed {
			out = append(out, Attention{"failed", j.Name, "the last run failed", link})
			continue
		}
		sc, err := ParseSchedule(j.Schedule)
		if err != nil || sc.IsManual() || sc.Kind == SchedAfter {
			continue
		}
		// Overdue: no success since the scheduled run before the last one
		// (so one run may still be in progress or just have failed).
		prev, ok := previousRun(sc, now, 2)
		if !ok {
			continue
		}
		c := cov[j.ID]
		switch {
		case c == nil || c.LastOK == nil:
			if j.CreatedAt.Before(prev) {
				out = append(out, Attention{"warning", j.Name, "no successful backup yet", link})
			}
		case c.LastOK.Before(prev.Add(-time.Hour)):
			out = append(out, Attention{"warning", j.Name, "last successful backup " + humanAge(now.Sub(*c.LastOK)) + " ago; scheduled runs since then did not succeed", link})
		}
	}
	agents, _ := s.store.ListAgents(ctx)
	for _, a := range agents {
		if a.Recovery {
			continue
		}
		if a.LastSeen == nil || now.Sub(*a.LastSeen) > 15*time.Minute {
			seen := "never connected"
			if a.LastSeen != nil {
				seen = "offline for " + humanAge(now.Sub(*a.LastSeen))
			}
			out = append(out, Attention{"warning", a.Hostname, "agent " + seen, "/agents"})
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Severity == "failed" && out[k].Severity != "failed" })
	return out
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// Protection counts what the backups protect, for the dashboard.
type Protection struct {
	Machines, VMs, Databases int
}

func (s *Server) protection(ctx context.Context) Protection {
	var p Protection
	cov, err := s.store.jobCoverage(ctx)
	if err != nil {
		return p
	}
	jobs, _ := s.store.ListJobs(ctx)
	machines := map[int64]bool{}
	vms, dbs := map[string]bool{}, map[string]bool{}
	for _, j := range jobs {
		c := cov[j.ID]
		if c == nil || c.LastOK == nil || j.Kind == JobCopy {
			continue
		}
		switch c.Unit {
		case "VM":
			for _, n := range c.Items {
				vms[fmt.Sprint(j.AgentID, j.VMwareHostID != nil, n)] = true
			}
		case "database":
			for _, n := range c.Items {
				dbs[fmt.Sprint(j.AgentID, n)] = true
			}
		default:
			machines[j.AgentID] = true
		}
	}
	p.Machines, p.VMs, p.Databases = len(machines), len(vms), len(dbs)
	return p
}

func jobKindTitle(k string) string {
	return map[string]string{JobFiles: "Files", JobImage: "Disk image", JobVM: "Virtual machines", JobSystem: "Linux system", JobCopy: "Backup copy", JobSQL: "SQL Server"}[k]
}

// jobKindCount is a filter chip of the job list.
type jobKindCount struct {
	Kind, Title string
	Count       int
}

func jobKindCounts(jobs []Job) []jobKindCount {
	var out []jobKindCount
	for _, k := range []string{JobFiles, JobImage, JobVM, JobSQL, JobSystem, JobCopy} {
		n := 0
		for _, j := range jobs {
			if j.Kind == k {
				n++
			}
		}
		if n > 0 {
			out = append(out, jobKindCount{k, jobKindTitle(k), n})
		}
	}
	return out
}

func failedJobs(jobs []Job) int {
	n := 0
	for _, j := range jobs {
		if j.LastStatus != nil && *j.LastStatus == api.StatusFailed {
			n++
		}
	}
	return n
}

// coverageMap is jobCoverage for templates (empty on errors).
func (s *Server) coverageMap(ctx context.Context) map[int64]*JobCoverage {
	m, err := s.store.jobCoverage(ctx)
	if err != nil {
		return map[int64]*JobCoverage{}
	}
	return m
}

// newestBackup is the newest successful full backup run of a job that
// lists what it protects (VM or SQL Server backups), or nil.
func (s *Store) newestBackup(ctx context.Context, jobID int64) (*Run, error) {
	rows, err := s.db.Query(ctx, `SELECT `+runCols+runFrom+` WHERE r.job_id=$1 AND r.kind IN ('vm-backup','sql-backup')
		AND r.status IN ('success','warning') AND r.details IS NOT NULL ORDER BY r.id DESC LIMIT 1`, jobID)
	if err != nil {
		return nil, err
	}
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// previousRun returns the n-th latest scheduled time before now (n=1: the
// last one), looking back at most 40 days.
func previousRun(sc Schedule, now time.Time, n int) (time.Time, bool) {
	var times []time.Time
	t := now.Add(-40 * 24 * time.Hour)
	for i := 0; i < 20000; i++ {
		t = sc.Next(t)
		if t.IsZero() || t.After(now) {
			break
		}
		times = append(times, t)
	}
	if len(times) < n {
		return time.Time{}, false
	}
	return times[len(times)-n], true
}
