package server

import (
	"context"
	"time"
)

// NoteScheduleMiss records a skipped schedule of a job (tests).
func (s *Store) NoteScheduleMiss(ctx context.Context, jobID int64, now time.Time) error {
	return s.noteScheduleMiss(ctx, jobID, missSkipped, now)
}
