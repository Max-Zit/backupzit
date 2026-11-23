package repo

import "time"

// The rebuild reads a pack up to three times, like the data check; tests
// do not wait for the pauses.
func init() { DefaultRetryPause = time.Millisecond }
