//go:build !windows

package imaging

import (
	"context"
	"errors"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
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

// Restore is not supported on this platform yet.
func Restore(context.Context, *repo.Repository, *repo.Snapshot, RestoreOptions) (*RestoreStats, error) {
	return nil, ErrUnsupported
}

// DetectFirmware is Windows only; elsewhere the firmware is unknown.
func DetectFirmware() Firmware { return Firmware{} }

// PrepareForNewHardware is Windows only.
func PrepareForNewHardware(context.Context, int, []string, func(string)) (*HardwareReport, error) {
	return nil, ErrUnsupported
}

// RecoveryDriverDirs is Windows only.
func RecoveryDriverDirs() []string { return nil }

// FinishLayout is Windows only.
func FinishLayout(context.Context, int, *RestoreStats, func(string)) []string { return nil }

// RebuildBoot is Windows only.
func RebuildBoot(context.Context, int, func(string)) (*BootReport, error) {
	return nil, ErrUnsupported
}
