package imaging

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

// dataDisk is a 100 GiB GPT disk with EFI, MSR, C: (40 GiB) and D: (20 GiB),
// then 39 GiB free.
func dataDisk() *Disk {
	return &Disk{Number: 1, Size: 100 * gib, SectorSize: 512, Style: StyleGPT, Partitions: []Partition{
		{Number: 1, Offset: 1 * mib, Length: 100 * mib, GPTType: GPTTypeEFI, GPTID: "11111111-1111-1111-1111-111111111111"},
		{Number: 2, Offset: 101 * mib, Length: 16 * mib, GPTType: GPTTypeMSR},
		{Number: 3, Offset: 117 * mib, Length: 40 * gib, GPTType: GPTTypeBasicData, GPTID: "33333333-3333-3333-3333-333333333333"},
		{Number: 4, Offset: 117*mib + 40*gib, Length: 20 * gib, GPTType: GPTTypeBasicData},
	}}
}

func TestPlanPartitionRestoreReplace(t *testing.T) {
	img := windowsGPT() // partition 3: Windows, ~59 GiB, NTFS
	d := dataDisk()
	w, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 3, Replace: 3})
	if err != nil {
		t.Fatal(err)
	}
	if w.Offset != 117*mib || w.Length != 40*gib || !w.Shrink {
		t.Errorf("replace keeps the slot and shrinks: %+v", w)
	}
	// D: right behind C: limits how far C: can grow.
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 3, Replace: 3, Length: 45 * gib}); err == nil {
		t.Error("growing into the next partition accepted")
	}
	// D: is last: it can grow into the free space behind it.
	w, err = planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 3, Replace: 4, Length: 59*gib + 512*mib})
	if err != nil {
		t.Fatal(err)
	}
	if w.Shrink || w.Length != 59*gib+512*mib {
		t.Errorf("grow into free space: %+v", w)
	}
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 3, Replace: 9}); err == nil {
		t.Error("missing target partition accepted")
	}
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 2, Replace: 4}); err == nil {
		t.Error("partition that was not backed up accepted")
	}
	// The EFI partition cannot be shrunk.
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 1, Replace: 1, Length: 50 * mib}); !errors.Is(err, ErrShrinkUnsupported) {
		t.Errorf("shrink EFI: %v", err)
	}
}

func TestPlanPartitionRestoreFree(t *testing.T) {
	img := windowsGPT()
	d := dataDisk()
	w, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 4})
	if err != nil {
		t.Fatal(err)
	}
	if w.Offset != 117*mib+60*gib || w.Length != 600*mib {
		t.Errorf("free space: %+v", w)
	}
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 3}); err == nil {
		t.Error("59 GiB into 39 GiB of free space accepted")
	}
	w, err = planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 3, Length: 30 * gib})
	if err != nil || !w.Shrink {
		t.Errorf("shrunk into free space: %+v, %v", w, err)
	}
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 4, Offset: 2 * gib}); err == nil {
		t.Error("offset inside C: accepted")
	}
	// A GPT recovery partition does not go on an MBR disk.
	mbr := &Disk{Number: 2, Size: 100 * gib, SectorSize: 512, Style: StyleMBR}
	if _, err := planPartitionRestore(img, mbr, PartitionRestoreOptions{Partition: 4}); err == nil {
		t.Error("GPT recovery partition on MBR accepted")
	}
	if _, err := planPartitionRestore(img, mbr, PartitionRestoreOptions{Partition: 3}); err != nil {
		t.Errorf("Windows partition on MBR: %v", err)
	}
	raw := &Disk{Number: 3, Size: 100 * gib, SectorSize: 512, Style: StyleRaw}
	if _, err := planPartitionRestore(img, raw, PartitionRestoreOptions{Partition: 3}); err == nil {
		t.Error("disk without partition table accepted")
	}
}

func gptLayout(entries ...[3]uint64) []byte {
	b := make([]byte, layoutHeader+len(entries)*layoutEntry)
	binary.LittleEndian.PutUint32(b[0:], layoutStyleGPT)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(entries)))
	for i, e := range entries {
		x := b[layoutHeader+i*layoutEntry:]
		binary.LittleEndian.PutUint32(x[0:], layoutStyleGPT)
		binary.LittleEndian.PutUint64(x[8:], e[0])
		binary.LittleEndian.PutUint64(x[16:], e[1])
		binary.LittleEndian.PutUint32(x[24:], uint32(e[2]))
	}
	return b
}

func TestSetLayoutEntryGPT(t *testing.T) {
	src := &repo.PartitionImage{Number: 3, GPTType: GPTTypeBasicData, Name: "Basic data partition", FileSystem: "NTFS"}
	raw := gptLayout([3]uint64{mib, 100 * mib, 1}, [3]uint64{117 * mib, 40 * gib, 3})
	w := partitionWrite{Src: src, Offset: 117 * mib, Length: 30 * gib}
	out, err := setLayoutEntry(raw, w, 3, "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", 512)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(raw) {
		t.Fatal("replace changed the entry count")
	}
	e := out[layoutHeader+layoutEntry:]
	if binary.LittleEndian.Uint64(e[16:]) != 30*gib || e[28] != 1 {
		t.Errorf("entry not rewritten")
	}
	id, _ := guidBytesPortable("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
	if string(e[48:64]) != string(id[:]) {
		t.Error("partition ID not set")
	}
	if binary.LittleEndian.Uint64(out[layoutHeader+16:]) != 100*mib {
		t.Error("other entry changed")
	}
	add, err := setLayoutEntry(raw, partitionWrite{Src: src, Offset: 60 * gib, Length: gib}, 0, "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", 512)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(add[4:]) != 3 || len(add) != layoutHeader+3*layoutEntry {
		t.Error("entry not added")
	}
	if _, err := setLayoutEntry(raw, w, 7, "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", 512); err == nil {
		t.Error("missing partition accepted")
	}
}

func TestSetLayoutEntryMBR(t *testing.T) {
	b := make([]byte, layoutHeader+4*layoutEntry)
	binary.LittleEndian.PutUint32(b[0:], layoutStyleMBR)
	binary.LittleEndian.PutUint32(b[4:], 4)
	first := b[layoutHeader:]
	binary.LittleEndian.PutUint64(first[8:], mib)
	binary.LittleEndian.PutUint64(first[16:], 50*gib)
	binary.LittleEndian.PutUint32(first[24:], 1)
	first[32], first[33] = 0x07, 1 // NTFS, active
	src := &repo.PartitionImage{Number: 3, GPTType: GPTTypeBasicData, FileSystem: "NTFS"}

	out, err := setLayoutEntry(b, partitionWrite{Src: src, Offset: mib, Length: 50 * gib}, 1, "", 512)
	if err != nil {
		t.Fatal(err)
	}
	if e := out[layoutHeader:]; e[32] != 0x07 || e[33] != 1 || binary.LittleEndian.Uint32(e[36:]) != 2048 {
		t.Errorf("replaced MBR entry: type %#x boot %d hidden %d", e[32], e[33], binary.LittleEndian.Uint32(e[36:]))
	}
	out, err = setLayoutEntry(b, partitionWrite{Src: &repo.PartitionImage{FileSystem: "FAT32"}, Offset: 51 * gib, Length: gib}, 0, "", 512)
	if err != nil {
		t.Fatal(err)
	}
	if e := out[layoutHeader+layoutEntry:]; e[32] != 0x0C || e[33] != 0 {
		t.Errorf("added MBR entry: type %#x boot %d", e[32], e[33])
	}
}

func TestPartitionIDInUse(t *testing.T) {
	disks := []Disk{*dataDisk()}
	if !partitionIDInUse(disks, "33333333-3333-3333-3333-333333333333", 9, 0) {
		t.Error("used ID not found")
	}
	if partitionIDInUse(disks, "33333333-3333-3333-3333-333333333333", 1, 3) {
		t.Error("the replaced partition itself counted")
	}
}

func TestGUIDBytesPortable(t *testing.T) {
	g, err := guidBytesPortable("C12A7328-F81F-11D2-BA4B-00A0C93EC93B")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B}
	if string(g[:]) != string(want) {
		t.Errorf("EFI type GUID bytes % x", g)
	}
	if _, err := guidBytesPortable("nope"); err == nil {
		t.Error("invalid GUID accepted")
	}
}

// On the disk of the running Windows its system partition (EFI, or the
// active one on MBR) holds the boot files in use: it is not replaced.
func TestPlanPartitionRestoreKeepsRunningSystemPartition(t *testing.T) {
	img := windowsGPT()
	d := dataDisk()
	d.System = true
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 1, Replace: 1}); err == nil || !strings.Contains(err.Error(), "running Windows") {
		t.Errorf("EFI partition of the running Windows: %v", err)
	}
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 3, Replace: 4}); err != nil {
		t.Errorf("data partition on the system disk: %v", err)
	}
	d.System = false
	if _, err := planPartitionRestore(img, d, PartitionRestoreOptions{Partition: 1, Replace: 1}); err != nil {
		t.Errorf("EFI partition of another disk: %v", err)
	}
	m := &Disk{Number: 0, Size: 100 * gib, SectorSize: 512, Style: StyleMBR, System: true, Partitions: []Partition{
		{Number: 1, Offset: 1 * mib, Length: 500 * mib, MBRType: 0x07, Bootable: true},
		{Number: 2, Offset: 501 * mib, Length: 50 * gib, MBRType: 0x07},
	}}
	if _, err := planPartitionRestore(img, m, PartitionRestoreOptions{Partition: 3, Replace: 1}); err == nil || !strings.Contains(err.Error(), "running Windows") {
		t.Errorf("active partition of the running Windows: %v", err)
	}
}

// Linux partitions stay Linux partitions when the table style changes.
func TestLinuxTypesOnConversion(t *testing.T) {
	for mbr, gpt := range linuxTypes {
		if got := gptTypeFor(&repo.PartitionImage{MBRType: mbr}); got != gpt {
			t.Errorf("MBR 0x%02X -> %s, want %s", mbr, got, gpt)
		}
		if got := mbrTypeFor(&repo.PartitionImage{GPTType: strings.ToLower(gpt)}); got != mbr {
			t.Errorf("GPT %s -> 0x%02X, want 0x%02X", gpt, got, mbr)
		}
	}
	if got := mbrTypeFor(&repo.PartitionImage{GPTType: GPTTypeBasicData, FileSystem: "NTFS"}); got != 0x07 {
		t.Errorf("NTFS -> 0x%02X", got)
	}
}
