package sysbackup

import (
	"context"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/repo"
)

// BackupOptions configure a system backup.
type BackupOptions struct {
	Excludes []string
	Version  string
	Tags     []string
	Progress func(path string, s *repo.SnapshotStats)
}

// Backup captures the disk layout and backs up every local file system.
func Backup(ctx context.Context, r *repo.Repository, opts BackupOptions) (*repo.Snapshot, error) {
	lay, paths, err := Capture(ctx, r)
	if err != nil {
		return nil, err
	}
	var extra []string
	for _, p := range paths {
		if p != "/" {
			extra = append(extra, p)
		}
	}
	return archiver.Run(ctx, r, archiver.Options{
		Paths:         []string{"/"},
		ExtraMounts:   extra,
		Excludes:      opts.Excludes,
		Version:       opts.Version,
		Tags:          append([]string{"system"}, opts.Tags...),
		Progress:      opts.Progress,
		OneFileSystem: true,
		System:        lay,
	})
}
