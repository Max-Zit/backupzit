package archiver

import (
	"context"
	"time"

	"github.com/max-zit/backupzit/internal/apfssnap"
)

// snapshotItem names snapshot problems in the run's errors.
const snapshotItem = "apfs snapshot"

// createSnapshots takes an APFS local snapshot of the data volume and reads
// the sources from it; paths on other volumes, or all of them when the
// snapshot fails, are read live and reported through onErr.
func createSnapshots(ctx context.Context, paths []string, timeout time.Duration, onErr func(item string, err error)) snapshotSet {
	snaps, _ := apfssnap.Create(ctx, paths, timeout, onErr)
	return snaps
}
