package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

func TestLoadGuestSnapshot(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := loadGuestSnapshot(ctx, r, "latest", "hyperv", 1); err == nil || !strings.Contains(err.Error(), "no snapshots") {
		t.Fatalf("empty repository: err = %v", err)
	}

	tree, err := r.SaveTree(ctx, &repo.Tree{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// One backup per VM, as 'hyperv backup --vmid N' writes them: the newest
	// snapshot holds VM 2, then a Proxmox guest with the same VMID as the
	// Hyper-V VM 1 (a copy of another host's backups).
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	save := func(minute int, guests ...repo.Guest) *repo.Snapshot {
		sn := &repo.Snapshot{Time: base.Add(time.Duration(minute) * time.Minute), Tree: tree, Guests: guests}
		if _, err := r.SaveSnapshot(ctx, sn); err != nil {
			t.Fatal(err)
		}
		return sn
	}
	hv1old := save(0, repo.Guest{Platform: "hyperv", VMID: 1})
	hv1 := save(1, repo.Guest{Platform: "hyperv", VMID: 1})
	pve1 := save(2, repo.Guest{Platform: "proxmox", VMID: 1})
	hv2 := save(3, repo.Guest{Platform: "hyperv", VMID: 2}, repo.Guest{Platform: "hyperv", VMID: 3})

	for _, tc := range []struct {
		name     string
		ref      string
		platform string
		vmid     int
		want     *repo.Snapshot
		wantErr  string
	}{
		// Newer snapshots hold VM 2 and a Proxmox guest 1: both are skipped.
		{name: "newest of the VM, not of the repository", ref: "latest", platform: "hyperv", vmid: 1, want: hv1},
		{name: "any platform", ref: "latest", vmid: 1, want: pve1},
		{name: "snapshot with several VMs", ref: "latest", platform: "hyperv", vmid: 3, want: hv2},
		{name: "VM in no snapshot", ref: "latest", platform: "hyperv", vmid: 4, wantErr: "contains guest 4"},
		{name: "explicit ID unchanged", ref: hv1old.ID.String(), platform: "hyperv", vmid: 2, want: hv1old},
		{name: "explicit prefix unchanged", ref: hv2.ID.Short(), platform: "vmware", vmid: 9, want: hv2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sn, err := loadGuestSnapshot(ctx, r, tc.ref, tc.platform, tc.vmid)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sn.ID != tc.want.ID {
				t.Errorf("snapshot = %s, want %s", sn.ID.Short(), tc.want.ID.Short())
			}
		})
	}
}
