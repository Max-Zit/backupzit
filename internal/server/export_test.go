package server

import (
	"context"
	"time"
)

// EnableUpdateHelper pretends the root update helper is installed (tests).
func (s *Server) EnableUpdateHelper() { s.upd.helper = true }

// PackageFormat is the package type the update installs on this machine.
var PackageFormat = packageFormat

// QueueDueSQLLogs is queueDueSQLLogs for tests.
func (s *Store) QueueDueSQLLogs(ctx context.Context, now time.Time) ([]int64, error) {
	return s.queueDueSQLLogs(ctx, now)
}
