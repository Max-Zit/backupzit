package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/backupzit/backupzit/internal/api"
)

// agentStatus summarises an agent's jobs for its tray app.
func (s *Server) agentStatus(ctx context.Context, agentID int64) (*api.AgentStatus, error) {
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	st := &api.AgentStatus{Jobs: []api.JobStatus{}}
	for _, j := range jobs {
		if j.AgentID != agentID {
			continue
		}
		js := api.JobStatus{ID: j.ID, Name: j.Name, Kind: j.Kind, Enabled: j.Enabled, Schedule: DescribeSchedule(j.Schedule)}
		if j.Enabled {
			if t := NextRun(j.Schedule, s.clock()); !t.IsZero() {
				js.NextRun = &t
			}
		}
		// The last finished run (the job's summary would show a running one).
		var status, msg string
		var finished *time.Time
		var active bool
		s.store.db.QueryRow(ctx, `SELECT COALESCE(l.status, ''), l.finished_at, COALESCE(l.message, ''),
			EXISTS (SELECT 1 FROM runs WHERE job_id=$1 AND kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log') AND status IN ('queued','running'))
			FROM (SELECT 1) one LEFT JOIN LATERAL (SELECT status, finished_at, message FROM runs WHERE job_id=$1
				AND kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log') AND status NOT IN ('queued','running')
				ORDER BY queued_at DESC LIMIT 1) l ON true`,
			j.ID).Scan(&status, &finished, &msg, &active)
		js.LastStatus, js.LastFinished, js.LastMessage, js.Running = status, finished, msg, active
		st.Jobs = append(st.Jobs, js)
	}
	return st, nil
}

// handleAgentJobRun serves POST /api/agent/jobs/{id}/run: "Back up now"
// from the tray app, for the agent's own jobs only.
func (s *Server) handleAgentJobRun(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil || j.AgentID != a.ID {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return
	}
	runID, err := s.store.QueueBackup(r.Context(), id, "agent")
	if errors.Is(err, ErrRunActive) {
		writeAPIError(w, http.StatusConflict, "this job is already queued or running")
		return
	} else if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not queue the backup")
		return
	}
	s.log.Info("backup started from the agent", "job", id, "run", runID, "agent", a.Hostname)
	writeJSON(w, http.StatusOK, map[string]int64{"run_id": runID})
}
