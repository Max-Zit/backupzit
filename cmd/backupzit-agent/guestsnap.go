package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/max-zit/backupzit/internal/repo"
)

// loadGuestSnapshot loads the snapshot that ref names. "latest" means the
// newest snapshot holding guest vmid on platform ("" = any platform), not
// the newest of the repository: with VMs backed up one by one into the same
// repository, the newest snapshot may hold another VM. The platform is the
// one the command's restore accepts, so both find the same guest.
func loadGuestSnapshot(ctx context.Context, r *repo.Repository, ref, platform string, vmid int) (*repo.Snapshot, error) {
	if ref != "latest" {
		return r.LoadSnapshot(ctx, ref)
	}
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	if len(sns) == 0 {
		return nil, errors.New("repository has no snapshots")
	}
	for i := len(sns) - 1; i >= 0; i-- {
		for _, g := range sns[i].Guests {
			if g.VMID == vmid && (platform == "" || g.Platform == platform) {
				return sns[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no snapshot in the repository contains guest %d", vmid)
}
