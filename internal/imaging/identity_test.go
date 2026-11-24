package imaging

import (
	"strings"
	"testing"
)

func identityDisks() []Disk {
	return []Disk{
		{Number: 0, Size: 64 << 30, SectorSize: 512, Model: "Virtual Disk", Style: StyleGPT, GPTDiskID: "AAAA",
			Partitions: []Partition{{Offset: 1 << 20, MountPoints: []string{`C:\`}, VolumeGUIDPath: `\\?\Volume{c0}\`}}},
		{Number: 2, Size: 512 << 30, SectorSize: 512, Model: "USB Disk", Serial: "S1", Bus: "usb", Style: StyleMBR, MBRSignature: 0x1234,
			Partitions: []Partition{
				{Offset: 1 << 20, MountPoints: []string{`E:\`}, VolumeGUIDPath: `\\?\Volume{e1}\`},
				{Offset: 100 << 30, MountPoints: []string{`C:\mnt\usb\`}, VolumeGUIDPath: `\\?\Volume{e2}\`},
			}},
	}
}

func TestDiskIdentityMatches(t *testing.T) {
	disks := identityDisks()
	id := disks[1].Identity()
	if err := id.Matches(disks[1]); err != nil {
		t.Fatal(err)
	}
	// Another disk under the same number (a re-plugged USB disk let the
	// repository disk take number 2).
	other := disks[0]
	other.Number = 2
	err := id.Matches(other)
	if err == nil || !strings.Contains(err.Error(), "disk 2 is no longer the disk that was chosen") || !strings.Contains(err.Error(), "size") {
		t.Errorf("another disk under the chosen number: %v", err)
	}
	// After the restore wrote a new table, the new identifiers count.
	written := disks[1]
	written.Style, written.GPTDiskID, written.MBRSignature = StyleGPT, "BBBB", 0
	if err := id.Matches(written); err == nil {
		t.Error("the old identity matches the rewritten disk")
	}
	if err := id.WithTable(StyleGPT, "bbbb", 0).Matches(written); err != nil {
		t.Errorf("identity with the new table: %v", err)
	}
	ref := DiskRef{Number: 2, ID: &id}
	if _, err := ref.Pick([]Disk{other}); err == nil {
		t.Error("Pick takes another disk with the number")
	}
	if d, err := ref.Pick(disks); err != nil || d.Number != 2 {
		t.Errorf("Pick = %v, %v", d, err)
	}
	if _, err := (DiskRef{Number: 2}).Pick([]Disk{other}); err != nil {
		t.Errorf("without an identity the number counts: %v", err)
	}
}

func TestDiskOfPath(t *testing.T) {
	disks := identityDisks()
	for path, want := range map[string]int{
		`E:\backups\pc1`:         2,
		`e:/backups`:             2,
		`\\?\E:\backups`:         2,
		`\\?\Volume{e1}\backups`: 2,
		`C:\mnt\usb\backups`:     2, // a folder mount point on the USB disk
		`C:\mnt\other`:           0,
		`C:\Windows`:             0,
		`\\?\Volume{C0}\x`:       0,
	} {
		if n, ok := DiskOfPath(disks, path); !ok || n != want {
			t.Errorf("DiskOfPath(%s) = %d, %v; want %d", path, n, ok, want)
		}
	}
	if _, ok := DiskOfPath(disks, `X:\Windows\System32\backupzit-recovery.exe`); ok {
		t.Error("a path on no disk (the RAM disk of Windows PE) maps to a disk")
	}
}
