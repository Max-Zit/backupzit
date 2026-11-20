package server_test

import (
	"time"

	"github.com/max-zit/backupzit/internal/checker"
)

// Data checks read a failing pack three times; tests need no pauses.
func init() { checker.DefaultRetryPause = time.Millisecond }
