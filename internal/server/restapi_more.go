package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// More REST API endpoints: one agent, its log, enabling and disabling jobs,
// storage usage and metrics in the Prometheus text format.

// lastGoodBackups is when each job last finished a backup or copy with
// success or warnings.
func (s *Store) lastGoodBackups(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := s.db.Query(ctx, `SELECT job_id, max(finished_at) FROM runs
		WHERE job_id IS NOT NULL AND status IN ('success','warning') AND kind = ANY($1) GROUP BY job_id`, timeLimitKinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var at *time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		if at != nil {
			out[id] = *at
		}
	}
	return out, rows.Err()
}

func (s *Server) registerAPIMore(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents/{id}", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		a, err := s.store.GetAgent(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return toAPIAgent(a), nil
	}))
	mux.HandleFunc("POST /api/v1/agents/{id}/log", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		a, err := s.store.GetAgent(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if agentOlder(a, minAgentLogVersion) {
			return nil, apiErr(http.StatusConflict, "the agent (%s) cannot send its log yet; update it to %s or newer", a.Version, minAgentLogVersion)
		}
		rid, err := s.store.QueueAgentLog(r.Context(), a.ID)
		if err != nil {
			return nil, apiErr(http.StatusConflict, "%s", err.Error())
		}
		s.audit(r, "agent.log", "requested the log of %s", a.Hostname)
		return map[string]int64{"run_id": rid}, nil
	}))
	mux.HandleFunc("GET /api/v1/runs/{id}/log", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		run, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if run.Kind != api.KindAgentLog {
			return nil, apiErr(http.StatusNotFound, "run %d is not an agent log", id)
		}
		text, err := s.store.AgentLog(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if text == "" {
			return nil, apiErr(http.StatusNotFound, "the agent has not sent its log yet (status %s)", run.Status)
		}
		return map[string]any{"run_id": id, "agent": run.Hostname, "log": text}, nil
	}))
	for _, x := range []struct {
		path    string
		enabled bool
	}{{"enable", true}, {"disable", false}} {
		mux.HandleFunc("POST /api/v1/jobs/{id}/"+x.path, s.api(PermJobs, func(w http.ResponseWriter, r *http.Request) (any, error) {
			id, err := pathInt(r, "id")
			if err != nil {
				return nil, err
			}
			j, err := s.store.GetJob(r.Context(), id)
			if err != nil {
				return nil, err
			}
			// With four-eyes approval a disabled job needs a second user,
			// which only the console can ask for.
			if !x.enabled && s.store.fourEyes(r.Context()) {
				return nil, apiErr(http.StatusConflict, "four-eyes approval is on: disable the job in the console, where a second user approves it")
			}
			if err := s.store.SetJobEnabled(r.Context(), id, x.enabled); err != nil {
				return nil, err
			}
			s.audit(r, "api.job_"+x.path, "job %d %s", id, j.Name)
			return map[string]any{"id": id, "enabled": x.enabled}, nil
		}))
	}
	mux.HandleFunc("GET /api/v1/storage", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		targets, err := s.store.ListTargets(r.Context())
		if err != nil {
			return nil, err
		}
		usage, err := s.store.storageUsage(r.Context(), time.Now())
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, t := range targets {
			x := map[string]any{"target_id": t.ID, "name": t.Name, "kind": t.Kind}
			if u := usage[t.ID]; u != nil {
				x["used_bytes"], x["repositories"] = u.Used, u.Repos
				if u.Total > 0 {
					x["total_bytes"], x["free_bytes"], x["used_percent"] = u.Total, u.Free, u.UsedPercent()
				}
				if u.Trend {
					x["growth_bytes_per_day"] = int64(u.GrowthPerDay)
				}
				if u.FullInDays >= 0 {
					x["full_in_days"] = u.FullInDays
				}
			}
			out = append(out, x)
		}
		return out, nil
	}))
	mux.HandleFunc("GET /api/v1/metrics", s.metrics)
}

// metrics serves GET /api/v1/metrics in the Prometheus text format (with
// an API token like the other endpoints).
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var text string
	h := s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		t, err := s.metricsText(r.Context(), time.Now())
		text = t
		return nil, err
	})
	rec := &metricsWriter{ResponseWriter: w}
	h(rec, r)
	if rec.status != 0 && rec.status >= 300 {
		return // the error was written as JSON
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(text))
}

// metricsWriter holds back the JSON body of a successful api() call, so
// metrics can write text instead; errors pass through.
type metricsWriter struct {
	http.ResponseWriter
	status int
}

func (m *metricsWriter) WriteHeader(code int) {
	m.status = code
	if code >= 300 {
		m.ResponseWriter.WriteHeader(code)
	}
}

func (m *metricsWriter) Write(b []byte) (int, error) {
	if m.status >= 300 {
		return m.ResponseWriter.Write(b)
	}
	return len(b), nil
}

func (s *Server) metricsText(ctx context.Context, now time.Time) (string, error) {
	sum, err := s.store.Summary(ctx)
	if err != nil {
		return "", err
	}
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return "", err
	}
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	good, err := s.store.lastGoodBackups(ctx)
	if err != nil {
		return "", err
	}
	usage, _ := s.store.storageUsage(ctx, now)
	targets, _ := s.store.ListTargets(ctx)

	var b strings.Builder
	metric := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	metric("backupzit_info", "Version of the console.", "gauge")
	fmt.Fprintf(&b, "backupzit_info{version=%q} 1\n", s.Version)
	metric("backupzit_agents", "Agents by state.", "gauge")
	fmt.Fprintf(&b, "backupzit_agents{state=\"online\"} %d\nbackupzit_agents{state=\"offline\"} %d\n", sum.AgentsOnline, sum.Agents-sum.AgentsOnline)
	metric("backupzit_runs_active", "Runs queued or running.", "gauge")
	fmt.Fprintf(&b, "backupzit_runs_active %d\n", sum.Running)
	metric("backupzit_runs_24h", "Runs of the last 24 hours by result.", "gauge")
	fmt.Fprintf(&b, "backupzit_runs_24h{status=\"failed\"} %d\nbackupzit_runs_24h{status=\"warning\"} %d\nbackupzit_runs_24h{status=\"all\"} %d\n", sum.Failed24h, sum.Warning24h, sum.Runs24h)
	metric("backupzit_suspicious_backups", "Suspicious backups (possible ransomware) not reviewed yet.", "gauge")
	fmt.Fprintf(&b, "backupzit_suspicious_backups %d\n", sum.Suspicious)

	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	metric("backupzit_agent_online", "1 if the agent polled the console recently.", "gauge")
	for _, a := range agents {
		fmt.Fprintf(&b, "backupzit_agent_online{agent_id=\"%d\",agent=%q,version=%q} %d\n", a.ID, a.Hostname, a.Version, boolInt(a.Online()))
	}
	metric("backupzit_agent_last_seen_timestamp_seconds", "When the agent last polled the console.", "gauge")
	for _, a := range agents {
		if a.LastSeen != nil {
			fmt.Fprintf(&b, "backupzit_agent_last_seen_timestamp_seconds{agent_id=\"%d\",agent=%q} %d\n", a.ID, a.Hostname, a.LastSeen.Unix())
		}
	}

	status := map[string]int{"success": 0, "warning": 1, "failed": 2}
	labels := func(j Job) string {
		return fmt.Sprintf("job_id=\"%d\",job=%q,kind=%q,agent=%q", j.ID, j.Name, j.Kind, j.Hostname)
	}
	metric("backupzit_job_enabled", "1 if the job runs on its schedule.", "gauge")
	for _, j := range jobs {
		fmt.Fprintf(&b, "backupzit_job_enabled{%s} %d\n", labels(j), boolInt(j.Enabled))
	}
	metric("backupzit_job_last_status", "Result of the job's last finished run: 0 success, 1 warning, 2 failed.", "gauge")
	for _, j := range jobs {
		if v, ok := status[deref(j.LastStatus)]; ok {
			fmt.Fprintf(&b, "backupzit_job_last_status{%s} %d\n", labels(j), v)
		}
	}
	metric("backupzit_job_last_success_timestamp_seconds", "When the job last finished a backup with success or warnings.", "gauge")
	for _, j := range jobs {
		if t, ok := good[j.ID]; ok {
			fmt.Fprintf(&b, "backupzit_job_last_success_timestamp_seconds{%s} %d\n", labels(j), t.Unix())
		}
	}
	metric("backupzit_storage_used_bytes", "Space the repositories take on a storage target.", "gauge")
	for _, t := range targets {
		if u := usage[t.ID]; u != nil {
			fmt.Fprintf(&b, "backupzit_storage_used_bytes{target_id=\"%d\",target=%q} %d\n", t.ID, t.Name, u.Used)
		}
	}
	metric("backupzit_storage_free_bytes", "Free space on a storage target, when it reports it.", "gauge")
	for _, t := range targets {
		if u := usage[t.ID]; u != nil && u.Total > 0 {
			fmt.Fprintf(&b, "backupzit_storage_free_bytes{target_id=\"%d\",target=%q} %d\n", t.ID, t.Name, u.Free)
		}
	}
	return b.String(), nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
