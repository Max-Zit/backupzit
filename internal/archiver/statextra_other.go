//go:build !windows && !darwin

package archiver

import (
	"syscall"

	"github.com/max-zit/backupzit/internal/repo"
)

// statExtra: Linux has no BSD file flags in stat and no creation time in
// Stat_t; the node keeps the format it always had.
func statExtra(*repo.UnixMeta, *syscall.Stat_t) {}
