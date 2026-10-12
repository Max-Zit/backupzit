package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
)

// Agent logs: the console asks an agent for its log (a run of kind
// agent-log); the agent posts the end of its log file and the newest
// installer logs, which can then be read and downloaded on the run page.

// minAgentLogVersion is the first agent version that sends its log.
const minAgentLogVersion = "0.38.0"

// agentLogDays is how long received logs are kept.
const agentLogDays = 30

// QueueAgentLog asks an agent for its log; it sends no notifications.
func (s *Store) QueueAgentLog(ctx context.Context, agentID int64) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, notified)
		SELECT $1,'agent-log','manual','',true
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE agent_id=$1 AND kind='agent-log' AND status IN ('queued','running'))
		RETURNING id`, agentID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("the log of this agent has already been requested")
	}
	return id, err
}

// AgentLog is the log a run received ("" if none).
func (s *Store) AgentLog(ctx context.Context, runID int64) (string, error) {
	var text string
	err := s.db.QueryRow(ctx, `SELECT data FROM run_logs WHERE run_id=$1`, runID).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return text, err
}

func (s *Store) saveAgentLog(ctx context.Context, agentID, runID int64, text string) error {
	ct, err := s.db.Exec(ctx, `INSERT INTO run_logs(run_id, data)
		SELECT id, $3 FROM runs WHERE id=$2 AND agent_id=$1 AND kind='agent-log' AND status='running'
		ON CONFLICT (run_id) DO UPDATE SET data=excluded.data, created_at=now()`, agentID, runID, strings.ToValidUTF8(strings.ReplaceAll(text, "\x00", ""), "?"))
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// pruneAgentLogs deletes received logs older than agentLogDays.
func (s *Store) pruneAgentLogs(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM run_logs WHERE created_at < now() - make_interval(days => $1)`, agentLogDays)
	return err
}

// handleAgentLogUpload serves POST /api/agent/runs/{id}/log.
func (s *Server) handleAgentLogUpload(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var l api.AgentLog
	if err := readJSON(r, &l); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.store.saveAgentLog(r.Context(), a.ID, id, l.Text); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "run not found or not running")
			return
		}
		s.log.Error("save agent log", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// handleAgentLogRequest serves POST /agents/{id}/log.
func (s *Server) handleAgentLogRequest(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		redirectErr(w, r, "/agents", errors.New("unknown agent"))
		return
	}
	if agentOlder(a, minAgentLogVersion) {
		redirectErr(w, r, "/agents", fmt.Errorf("the agent on %s (%s) cannot send its log yet; update it to %s or newer", a.Hostname, a.Version, minAgentLogVersion))
		return
	}
	rid, err := s.store.QueueAgentLog(r.Context(), a.ID)
	if err != nil {
		redirectErr(w, r, "/agents", err)
		return
	}
	s.audit(r, "agent.log", "requested the log of %s", a.Hostname)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "The agent sends its log on its next poll.")
}

// handleAgentLogDownload serves GET /runs/{id}/log.
func (s *Server) handleAgentLogDownload(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil || run.Kind != api.KindAgentLog {
		http.NotFound(w, r)
		return
	}
	text, err := s.store.AgentLog(r.Context(), id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if text == "" {
		http.NotFound(w, r)
		return
	}
	when := time.Now()
	if run.FinishedAt != nil {
		when = *run.FinishedAt
	}
	name := fmt.Sprintf("backupzit-agent-%s-%s.log", safeFileName(run.Hostname), when.Format("20060102-1504"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write([]byte(text))
}

// safeFileName keeps letters, digits, dots and dashes of a name.
func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// logTail is the last n lines of a log, for the run page.
func logTail(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
