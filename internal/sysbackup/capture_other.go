//go:build !linux

package sysbackup

import (
	"context"
	"errors"

	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
)

// ErrUnsupported is returned on systems other than Linux.
var ErrUnsupported = errors.New("system backup and restore are available on Linux; use disk images on Windows")

// Capture is Linux only.
func Capture(context.Context, *repo.Repository) (*repo.SystemLayout, []string, error) {
	return nil, nil, ErrUnsupported
}

// RestoreOptions control a system restore (Linux only).
type RestoreOptions struct {
	Target           string
	Disk             string
	NewHardware      bool
	DisableAgent     bool
	RebuildInitramfs bool
	Log              func(string)
	Progress         func(path string, s *restorer.Stats)
}

// RestoreResult describes the restored system.
type RestoreResult struct {
	Target      string   `json:"target"`
	Partitions  int      `json:"partitions"`
	FileSystems []string `json:"file_systems"`
	Files       uint64   `json:"files"`
	Bytes       uint64   `json:"bytes"`
	Boot        string   `json:"boot"`
	Notes       []string `json:"notes,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

// Restore is Linux only.
func Restore(context.Context, *repo.Repository, *repo.Snapshot, RestoreOptions) (*RestoreResult, error) {
	return nil, ErrUnsupported
}
