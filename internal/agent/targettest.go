package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
)

// targetTestTimeout bounds a connection test, as on the console: the user
// waits for it on the page.
var targetTestTimeout = 30 * time.Second

// targetTest tests the storage of the run's repository for the console
// (KindTargetTest): the console cannot reach storage on this computer's
// network (#63), so the agent does what the console's test does, with the
// same code (backend.CheckConnection). The message of a failed test is the
// failed step as it is: the console adds its hints to it.
func (a *Agent) targetTest(ctx context.Context, run api.Run) api.RunResult {
	ctx, cancel := context.WithTimeout(ctx, targetTestTimeout)
	defer cancel()
	rs := run.Repository
	opts, cleanup, err := backendOptions(rs)
	if err != nil {
		return api.RunResult{Status: api.StatusFailed, Message: err.Error()}
	}
	defer cleanup()
	be, err := backend.Open(ctx, rs.URL, opts)
	if err != nil {
		return api.RunResult{Status: api.StatusFailed, Message: fmt.Sprintf("connect: %v", err)}
	}
	defer be.Close()
	c, err := backend.CheckConnection(ctx, be, rs.URL, rs.S3LockDays)
	if err != nil {
		return api.RunResult{Status: api.StatusFailed, Message: err.Error()}
	}
	d := api.TargetTest{Immutable: c.Immutable, RetentionChecked: c.RetentionChecked, RetentionMode: c.RetentionMode, RetentionDays: c.RetentionDays}
	if c.RetentionErr != nil {
		d.RetentionError = c.RetentionErr.Error()
	}
	details, _ := json.Marshal(d)
	return api.RunResult{Status: api.StatusSuccess, Message: c.Summary(), Details: details}
}
