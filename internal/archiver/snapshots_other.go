//go:build !darwin

package archiver

import (
	"context"
	"time"

	"github.com/max-zit/backupzit/internal/vss"
)

// snapshotItem names snapshot problems in the run's errors.
const snapshotItem = "vss"

// createSnapshots makes VSS snapshots of the volumes of paths (Windows;
// elsewhere VSS reports itself unavailable and every path is read live).
func createSnapshots(_ context.Context, paths []string, timeout time.Duration, onErr func(item string, err error)) snapshotSet {
	snaps, _ := vss.Create(paths, timeout, onErr)
	return snaps
}
