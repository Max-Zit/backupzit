package checker_test

import (
	"time"

	"github.com/max-zit/backupzit/internal/checker"
)

// Damaged packs are read three times before the verdict; tests need no pauses.
func init() { checker.DefaultRetryPause = time.Millisecond }
