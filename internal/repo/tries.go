package repo

import (
	"context"
	"time"
)

// DefaultRetryPause is the wait before the second try of Tries when the
// caller gives none (10 s, doubled for the third try; tests shorten it).
var DefaultRetryPause = 10 * time.Second

// Tries runs read up to three times, pausing in between, while it fails.
// A proven verdict (ErrPackDamaged) is read again too: over plain HTTP a
// corrupt download can be a one-off. The data check and the index rebuild
// both use it.
func Tries(ctx context.Context, pause time.Duration, read func() error) error {
	if pause <= 0 {
		pause = DefaultRetryPause
	}
	var err error
	for i := 0; i < 3; i++ {
		if err = read(); err == nil || ctx.Err() != nil {
			return err
		}
		if i < 2 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(pause << i):
			}
		}
	}
	return err
}
