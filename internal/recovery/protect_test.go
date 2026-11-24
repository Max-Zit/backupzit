package recovery

import (
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// A usb:// repository (rotating backup disks, as the console writes it
// into access packages) protects the disk that has its label now.
func TestProtectedDisksUSBLocation(t *testing.T) {
	old := resolveUSB
	t.Cleanup(func() { resolveUSB = old })
	resolveUSB = func(loc string) (string, string, error) {
		if strings.HasPrefix(loc, "usb://BACKUP") {
			return `E:\backups\pc1`, "usb://BACKUP1/backups/pc1", nil
		}
		return "", "", backend.ErrNoDisk
	}
	p := ProtectedDisks(testDisks(), []string{"usb://BACKUP*/backups/pc1", "usb://OTHER/x"})
	if len(p) != 1 || p[1] != "usb://BACKUP*/backups/pc1" {
		t.Errorf("protected = %v, want disk 1 for the usb:// location", p)
	}
}

// A job whose disk number now belongs to another disk is refused.
func TestJobCheckTargetIdentity(t *testing.T) {
	sn := &repo.Snapshot{Images: []repo.DiskImage{{Style: "gpt", SectorSize: 512, Partitions: []repo.PartitionImage{
		{Number: 1, Offset: 1 << 20, Length: 100 << 20, Included: true},
	}}}}
	disks := testDisks()
	id := disks[0].Identity()
	j := Job{Snapshot: sn, TargetDisk: 0, TargetID: &id}
	if err := j.Check(disks, nil); err != nil {
		t.Fatalf("same disk: %v", err)
	}
	// The USB disk with the repository now has number 0.
	swapped := []imaging.Disk{disks[1], disks[0]}
	swapped[0].Number, swapped[1].Number = 0, 1
	err := j.Check(swapped, ProtectedDisks(swapped, []string{`E:\backups`}))
	if err == nil || !strings.Contains(err.Error(), "no longer the disk that was chosen") {
		t.Errorf("swapped disks: %v", err)
	}
}

// A disk converted to another partition style cannot stay offline: the
// steps that make it start would be skipped.
func TestJobCheckKeepOfflineConversion(t *testing.T) {
	img := repo.DiskImage{Style: "mbr", SectorSize: 512, Partitions: []repo.PartitionImage{
		{Number: 1, Offset: 1 << 20, Length: 100 << 20, Included: true, FileSystem: "NTFS"},
	}}
	sn := &repo.Snapshot{Images: []repo.DiskImage{img}}
	l := &imaging.Layout{Style: imaging.StyleGPT, DiskSize: 512 << 30, SectorSize: 512, Partitions: []imaging.LayoutPartition{
		{Source: imaging.SourceNewEFI, Offset: 1 << 20, Length: 100 << 20},
		{Source: 1, Offset: 101 << 20, Length: 100 << 20, Restore: true},
	}}
	j := Job{Snapshot: sn, TargetDisk: 0, Layout: l, KeepOffline: true}
	err := j.Check(testDisks(), nil)
	if err == nil || !strings.Contains(err.Error(), "cannot stay offline") {
		t.Errorf("conversion kept offline: %v", err)
	}
	j.KeepOffline = false
	if err := j.Check(testDisks(), nil); err != nil && strings.Contains(err.Error(), "offline") {
		t.Errorf("online conversion refused for being offline: %v", err)
	}
}
