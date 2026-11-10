package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupCheckpointOwnership(t *testing.T) {
	for _, scenario := range []string{"old-without-marker", "completed", "busy", "remove-error"} {
		t.Run(scenario, func(t *testing.T) {
			id := testVMGUID(t)
			disk := filepath.Join(t.TempDir(), "disk.vhdx")
			name := "backupzit_owned"
			marker := doneMarker(vmInfo{Disks: []vmDisk{{Path: disk}}}, name)
			if scenario != "old-without-marker" {
				if err := os.WriteFile(marker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "busy" {
				release := holdTestVMLock(t, id)
				defer release()
			}
			original := ps
			defer func() { ps = original }()
			deletes := 0
			failure := errors.New("injected checkpoint deletion failure")
			ps = func(_ context.Context, script string) ([]byte, error) {
				if strings.Contains(script, "Remove-VMSnapshot") {
					deletes++
					if !strings.Contains(script, psq(id)) || !strings.Contains(script, psq(name)) {
						t.Error("cleanup did not select the checkpoint's VM GUID and exact name")
					}
					if scenario == "remove-error" {
						return nil, failure
					}
					return nil, nil
				}
				return json.Marshal([]map[string]any{{"id": id, "vm": "test", "name": name, "disk": disk, "age": 999999}})
			}
			removed, err := CleanupCheckpoints(context.Background())
			switch scenario {
			case "completed":
				if err != nil || deletes != 1 || len(removed) != 1 {
					t.Fatalf("completed checkpoint: deleted=%d removed=%v err=%v", deletes, removed, err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("completion marker not removed")
				}
			case "remove-error":
				if !errors.Is(err, failure) || len(removed) != 0 {
					t.Fatalf("delete error lost: %v %v", removed, err)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal("marker removed after failed deletion")
				}
			default:
				if err != nil || deletes != 0 || len(removed) != 0 {
					t.Fatalf("unproven/busy checkpoint deleted: %d %v %v", deletes, removed, err)
				}
			}
			if scenario != "busy" {
				unlock, err := lockVM(context.Background(), id, false)
				if err != nil {
					t.Fatalf("cleanup leaked VM lock: %v", err)
				}
				unlock()
			}
		})
	}
}
