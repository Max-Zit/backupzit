package imaging

import (
	"fmt"
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

// #61 finding 2: a copy restored next to its source disk has that disk's
// IDs; the disk is found by style and ID, never the restored disk itself.
func TestDiskWithID(t *testing.T) {
	disks := []Disk{
		{Number: 0, Style: StyleGPT, GPTDiskID: "1E2C83A4-0000-4000-8000-0000000000AA"},
		{Number: 1, Style: StyleMBR, MBRSignature: 0xCAFE},
		{Number: 2, Style: StyleRaw},
		{Number: 6, Style: StyleGPT, GPTDiskID: "1e2c83a4-0000-4000-8000-0000000000aa"},
	}
	for _, c := range []struct {
		except    int
		style, id string
		sig       uint32
		want      int
		wantFound bool
		what      string
	}{
		{6, StyleGPT, "1E2C83A4-0000-4000-8000-0000000000AA", 0, 0, true, "GPT, any case"},
		{0, StyleGPT, "1E2C83A4-0000-4000-8000-0000000000AA", 0, 6, true, "the other way round"},
		{6, StyleGPT, "1E2C83A4-0000-4000-8000-0000000000AB", 0, 0, false, "another GPT ID"},
		{6, StyleMBR, "", 0xCAFE, 1, true, "MBR signature"},
		{1, StyleMBR, "", 0xCAFE, 0, false, "only the restored disk itself"},
		{6, StyleMBR, "", 0, 0, false, "signature 0 matches nothing"},
		{6, StyleGPT, "", 0, 0, false, "empty GPT ID matches nothing"},
		{6, StyleGPT, "", 0xCAFE, 0, false, "an MBR signature on a GPT table"},
	} {
		n, ok := DiskWithID(disks, c.except, c.style, c.id, c.sig)
		if ok != c.wantFound || n != c.want {
			t.Errorf("%s: got %d, %v; want %d, %v", c.what, n, ok, c.want, c.wantFound)
		}
	}
}

// The warning for a larger partition without a volume names the disk with
// the same IDs when there is one, and keeps the old text otherwise.
func TestNoVolumeWarning(t *testing.T) {
	id := DiskIdentity{Style: StyleGPT, GPTDiskID: "AAAA"}
	ref := DiskRef{Number: 6, ID: &id}
	part := &Partition{Number: 3, Offset: 227540992}
	disks := []Disk{{Number: 2, Style: StyleGPT, GPTDiskID: "aaaa"}, {Number: 6, Style: StyleGPT, GPTDiskID: "AAAA"}}
	got := noVolumeWarning(disks, ref, part, part.Offset)
	if want := fmt.Sprintf(SameIDOfflineWarning, 3, 2); got != want {
		t.Errorf("same IDs: %q, want %q", got, want)
	}
	for _, s := range []string{"disk 2", "same disk and partition IDs", "offline", "remove the old disk", "Disk Management"} {
		if !strings.Contains(got, s) {
			t.Errorf("same IDs: %q does not say %q", got, s)
		}
	}
	for what, c := range map[string]struct {
		disks []Disk
		ref   DiskRef
		part  *Partition
	}{
		"no other disk with the IDs": {disks[1:], ref, part},
		"partition not listed":       {disks, ref, nil},
		"no identity":                {disks, DiskRef{Number: 6}, part},
	} {
		if got := noVolumeWarning(c.disks, c.ref, c.part, 227540992); got != "partition at 227540992: no volume to extend; extend it in Disk Management" {
			t.Errorf("%s: %q", what, got)
		}
	}
}
