package agent

import (
	"time"

	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
)

// Data checks read a failing pack three times; tests need no pauses.
func init() {
	checker.DefaultRetryPause = time.Millisecond
	repo.DefaultRetryPause = time.Millisecond
}
