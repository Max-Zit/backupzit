package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/api"
)

// Runs left "running" when an agent is restarted in the middle of them: the
// new process does not know them and never finishes them, and they block
// console updates until someone stops them. Agents 0.52.0 and newer send the
// IDs of the runs they work on with every poll; the agent's running runs
// missing from that list are failed.

// orphanGrace spares runs just handed out (the answer to the poll may not
// have reached the agent yet). An agent keeps a run in its list until the
// result was delivered or given up, so a slow result is never cut short.
var orphanGrace = 2 * time.Minute

const orphanMessage = "The agent is no longer running this (it was restarted or stopped, or could not deliver the result); see the agent's log for what happened"

// orphanedRuns lists the running runs of an agent that are not in active and
// started more than grace ago.
func (s *Store) orphanedRuns(ctx context.Context, agentID int64, active []int64, grace time.Duration) ([]int64, error) {
	if active == nil {
		active = []int64{}
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM runs WHERE agent_id=$1 AND status='running'
		AND NOT (id = ANY($2)) AND started_at < now() - make_interval(secs => $3) ORDER BY id`,
		agentID, active, grace.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// failOrphanedRuns fails the runs an agent no longer works on, as if the
// agent had reported them failed (chained jobs follow as after a failure; a
// run already being cancelled ends as cancelled).
func (s *Server) failOrphanedRuns(ctx context.Context, a Agent, active []int64) {
	ids, err := s.store.orphanedRuns(ctx, a.ID, active, orphanGrace)
	if err != nil {
		s.log.Error("orphaned runs", "agent", a.Hostname, "err", err)
		return
	}
	for _, id := range ids {
		if err := s.store.FinishRun(ctx, a.ID, id, api.RunResult{Status: api.StatusFailed, Message: orphanMessage}); err != nil {
			continue // finished meanwhile
		}
		s.log.Warn("run failed: the agent is no longer running it", "run", id, "agent", a.Hostname)
		s.chainAfterRun(ctx, id)
	}
}
