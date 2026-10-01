package server

import (
	"testing"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/repo"
)

func TestDetectAnomaly(t *testing.T) {
	normal := repo.SnapshotStats{Files: 1000, FilesChanged: 10, FilesNew: 2, BytesAdded: 20 << 20, BytesStored: 8 << 20}
	hist := []repo.SnapshotStats{normal, normal, normal}
	if r := detectAnomaly(api.KindBackup, normal, hist); len(r) != 0 {
		t.Errorf("normal backup flagged: %v", r)
	}
	if r := detectAnomaly(api.KindBackup, normal, hist[:2]); len(r) != 0 {
		t.Error("flagged without enough history")
	}
	encrypted := repo.SnapshotStats{Files: 1000, FilesChanged: 950, BytesAdded: 900 << 20, BytesStored: 899 << 20}
	if r := detectAnomaly(api.KindBackup, encrypted, hist); len(r) < 2 {
		t.Errorf("ransomware not detected: %v", r)
	}
	deleted := repo.SnapshotStats{Files: 400, FilesChanged: 1, BytesAdded: 1 << 20, BytesStored: 1 << 19}
	if r := detectAnomaly(api.KindBackup, deleted, hist); len(r) != 1 {
		t.Errorf("mass deletion not detected: %v", r)
	}
	img := repo.SnapshotStats{BytesAdded: 30 << 30, BytesStored: 30 << 30}
	if r := detectAnomaly(api.KindImageBackup, img, hist); len(r) == 0 {
		t.Error("image data spike not detected")
	}
}
