package server

import (
	"context"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/netcfg"
)

// EnableUpdateHelper pretends the root update helper is installed (tests).
func (s *Server) EnableUpdateHelper() { s.upd.helper = true }

// PackageFormat is the package type the update installs on this machine.
var PackageFormat = packageFormat

// QueueDueSQLLogs is queueDueSQLLogs for tests.
func (s *Store) QueueDueSQLLogs(ctx context.Context, now time.Time) ([]int64, error) {
	return s.queueDueSQLLogs(ctx, now)
}

// APIRun is what the agent gets for a run (tests).
func (s *Server) APIRun(ctx context.Context, runID int64) (*api.Run, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	return s.toAPIRun(ctx, &run)
}

// UseNetSystem replaces the system whose network settings the console shows.
func UseNetSystem(s *netcfg.System) func() {
	saved := netSystem
	netSystem = s
	return func() { netSystem = saved }
}
