package backend

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestResolveUSB(t *testing.T) {
	vols := []Volume{{"System", `C:\`}, {"BZBACKUP2", filepath.FromSlash("/media/bz2")}, {"Photos", `F:\`}}
	listVolumesFn = func() ([]Volume, error) { return vols, nil }
	defer func() { listVolumesFn = listVolumes }()

	dir, loc, err := ResolveUSB("usb://bzbackup*/BackupZit/pc1_ab")
	if err != nil || dir != filepath.Join(filepath.FromSlash("/media/bz2"), "BackupZit", "pc1_ab") || loc != "usb://BZBACKUP2/BackupZit/pc1_ab" {
		t.Fatalf("resolve: %q %q %v", dir, loc, err)
	}
	if _, _, err := ResolveUSB("usb://BZBACKUP1/BackupZit/pc1_ab"); !errors.Is(err, ErrNoDisk) {
		t.Errorf("other disk: %v", err)
	}
	vols = append(vols, Volume{"BZBACKUP3", `G:\`})
	if _, _, err := ResolveUSB("usb://BZBACKUP*/x"); err == nil {
		t.Error("two matching disks accepted")
	}
	if USBLabel("usb://BZBACKUP2/BackupZit/x") != "BZBACKUP2" {
		t.Error("label")
	}
}
