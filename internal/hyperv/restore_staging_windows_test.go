package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

// These tests require Windows but no Hyper-V role/VM: only inventory and
// New-VHD are substituted. The write-failure case calls the real Virtual Disk
// API with an invalid VHD, exercising cleanup before any VM mutation.
func TestRestorePreparationFailure(t *testing.T) {
	for _, scenario := range []string{"missing-image", "missing-second-image", "empty-disks", "invalid-key", "duplicate-key", "size-mismatch", "new-vhd", "write-vhd", "canceled", "clustered-new-vhd"} {
		t.Run(scenario, func(t *testing.T) {
			folder := t.TempDir()
			originalPath := filepath.Join(folder, "original.vhdx")
			if err := os.WriteFile(originalPath, []byte("original disk sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			vm := vmInfo{ID: "a64fb4a9-6ab2-4437-ac94-dc1964d2eca2", Name: "fixture", Generation: 2, State: "Running", Disks: []vmDisk{{Type: "SCSI", Path: originalPath}}, Clustered: strings.HasPrefix(scenario, "clustered-")}
			cfg, _ := json.Marshal(vm)
			sn := &repo.Snapshot{Guests: []repo.Guest{{Platform: "hyperv", VMID: 42, ID: vm.ID, Config: string(cfg), Disks: []repo.GuestDisk{{Key: "scsi0:0", Image: 0, Size: 64 << 20}}}}, Images: []repo.DiskImage{{Size: 64 << 20}}}
			switch scenario {
			case "missing-image":
				sn.Guests[0].Disks[0].Image = -1
			case "missing-second-image":
				sn.Guests[0].Disks = append(sn.Guests[0].Disks, repo.GuestDisk{Key: "scsi0:1", Image: 1, Size: 64 << 20})
			case "empty-disks":
				sn.Guests[0].Disks = nil
			case "invalid-key":
				sn.Guests[0].Disks[0].Key = "../outside"
			case "duplicate-key":
				sn.Guests[0].Disks = append(sn.Guests[0].Disks, sn.Guests[0].Disks[0])
			case "size-mismatch":
				sn.Images[0].Size++
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			originalPS := ps
			defer func() { ps = originalPS }()
			ps = func(_ context.Context, script string) ([]byte, error) {
				if strings.Contains(script, psVMInfo) {
					return cfg, nil
				}
				if strings.HasPrefix(script, "New-VHD -Path ") {
					paths, err := filepath.Glob(filepath.Join(folder, "backupzit-restore-*"))
					if err != nil || len(paths) != 1 {
						t.Fatalf("expected unique staging directory: %v, %v", paths, err)
					}
					if err := os.WriteFile(filepath.Join(paths[0], "scsi0-0.vhdx"), []byte("partial or invalid VHD"), 0o600); err != nil {
						t.Fatal(err)
					}
					if scenario == "canceled" {
						cancel()
						return nil, ctx.Err()
					}
					if scenario != "write-vhd" {
						return nil, errors.New("injected New-VHD failure")
					}
					return nil, nil
				}
				// Any stop, checkpoint merge, cluster action or reconfiguration
				// here means preparation has already disturbed the original VM.
				t.Errorf("unexpected VM mutation during failed preparation: %s", script)
				return nil, errors.New("unexpected PowerShell call")
			}
			_, err := Restore(ctx, nil, sn, RestoreOptions{VMID: 42, Overwrite: true, Cluster: vm.Clustered})
			if err == nil {
				t.Fatal("expected preparation failure")
			}
			entries, err := os.ReadDir(folder)
			if err != nil || len(entries) != 1 || entries[0].Name() != "original.vhdx" {
				t.Fatalf("unexpected files after failed preparation: %v, %v", entries, err)
			}
			data, err := os.ReadFile(originalPath)
			if err != nil || string(data) != "original disk sentinel" {
				t.Fatalf("original disk changed: %q, %v", data, err)
			}
		})
	}
}
