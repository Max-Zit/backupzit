package server_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// A16: starts of the same job at the same moment queue one run.
func TestQueueRace(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs", Paths: []string{t.TempDir()}, Enabled: true})
	for round := 0; round < 3; round++ {
		const n = 16
		var wg sync.WaitGroup
		var mu sync.Mutex
		queued, active := 0, 0
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := e.store.QueueBackup(ctx, jobID, "manual")
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					queued++
				case errors.Is(err, server.ErrRunActive):
					active++
				default:
					t.Errorf("queue: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if queued != 1 || active != n-1 {
			t.Fatalf("round %d: %d runs queued, %d refused; want 1", round, queued, active)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE runs SET status='cancelled' WHERE job_id=$1`, jobID); err != nil {
			t.Fatal(err)
		}
	}
}
