//go:build !windows

package imaging

import (
	"context"
	"errors"
	"time"

	"github.com/backupzit/backupzit/internal/repo"
)

// ErrUnsupported is returned on platforms without image support yet.
var ErrUnsupported = errors.New("disk imaging is currently only supported on Windows")

// ListDisks returns all physical disks.
func ListDisks() ([]Disk, error) { return nil, ErrUnsupported }

// BackupOptions configure an image backup.
type BackupOptions struct {
	Disk       int
	Partitions []int
	VSS        bool
	VSSTimeout time.Duration
	Hostname   string
	Version    string
	Tags       []string
	Progress   func(done, total uint64)
}

// Backup is not supported on this platform yet.
func Backup(context.Context, *repo.Repository, BackupOptions) (*repo.Snapshot, error) {
	return nil, ErrUnsupported
}

// RestoreOptions configure an image restore.
type RestoreOptions struct {
	TargetDisk  int
	Image       int
	Progress    func(done, total uint64)
	KeepOffline bool
}

// RestoreStats summarises an image restore.
type RestoreStats struct {
	Partitions   int
	BytesWritten uint64
	Duration     time.Duration
}

// Restore is not supported on this platform yet.
func Restore(context.Context, *repo.Repository, *repo.Snapshot, RestoreOptions) (*RestoreStats, error) {
	return nil, ErrUnsupported
}

// HardwareReport describes what PrepareForNewHardware changed.
type HardwareReport struct {
	WindowsVolume  string   `json:"windows_volume"`
	DriversEnabled []string `json:"drivers_enabled"`
	DriversAdded   int      `json:"drivers_added"`
	DriverSources  []string `json:"driver_sources,omitempty"`
	BootRebuilt    bool     `json:"boot_rebuilt"`
	Warnings       []string `json:"warnings,omitempty"`
}

// PrepareForNewHardware is Windows only.
func PrepareForNewHardware(context.Context, int, []string, func(string)) (*HardwareReport, error) {
	return nil, ErrUnsupported
}

// RecoveryDriverDirs is Windows only.
func RecoveryDriverDirs() []string { return nil }
