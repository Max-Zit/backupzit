//go:build !windows

// Package hyperv backs up Hyper-V VMs from the agent on a Hyper-V host
// (Windows only).
package hyperv

import (
	"context"
	"errors"

	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/repo"
)

var errUnsupported = errors.New("Hyper-V backups run on a Windows Hyper-V host")

func Available() bool { return false }

func VMIDFor(string) int { return 0 }

func GetInventory(context.Context) (*pve.Inventory, error) { return nil, errUnsupported }

type BackupOptions struct {
	VMIDs    []int
	Exclude  []int
	Hostname string
	Version  string
	Tags     []string
	Progress func(done, total uint64)
	Log      func(msg string, args ...any)
}

func Backup(context.Context, *repo.Repository, BackupOptions) (*repo.Snapshot, error) {
	return nil, errUnsupported
}

type RestoreOptions struct {
	VMID      int
	AsNew     bool
	Name      string
	Folder    string
	Overwrite bool
	Start     bool
	Progress  func(done, total uint64)
	Log       func(msg string, args ...any)
}

type RestoreResult struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Host    string   `json:"host"`
	Disks   []string `json:"disks"`
	Bytes   uint64   `json:"bytes"`
	Started bool     `json:"started,omitempty"`
	NewMACs bool     `json:"new_macs,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

func Restore(context.Context, *repo.Repository, *repo.Snapshot, RestoreOptions) (*RestoreResult, error) {
	return nil, errUnsupported
}

func RCTInfo(string, string) (bool, string, uint64, int, error) {
	return false, "", 0, 0, errUnsupported
}
