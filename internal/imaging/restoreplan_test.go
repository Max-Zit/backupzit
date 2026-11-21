package imaging

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

func TestRestorePlanAsImage(t *testing.T) {
	img := windowsGPT()
	plan, err := restorePlan(img, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range plan {
		if w.Moved() || w.Restore != img.Partitions[i].Included {
			t.Errorf("partition %d: %+v", w.Src.Number, w)
		}
	}
	plan, err = restorePlan(img, RestoreOptions{Partitions: []int{3}})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range plan {
		if w.Restore != (w.Src.Number == 3) {
			t.Errorf("partition %d restore=%v", w.Src.Number, w.Restore)
		}
	}
	if _, err := restorePlan(img, RestoreOptions{Partitions: []int{2}}); err == nil {
		t.Error("partition that was not backed up accepted")
	}
}

func TestRestorePlanLayout(t *testing.T) {
	img := windowsGPT()
	lim := windowsLimits(10 * gib)
	l, err := SuggestLayout(img, 100*gib, lim, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := restorePlan(img, RestoreOptions{Layout: l})
	if err != nil {
		t.Fatal(err)
	}
	if !plan[2].Grown() || plan[0].Moved() || !plan[3].Moved() {
		t.Errorf("plan: %+v", plan)
	}
	// NTFS partitions shrink; others are refused.
	small, err := SuggestLayout(img, 40*gib, lim, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := restorePlan(img, RestoreOptions{Layout: small})
	if err != nil || !sp[2].Shrink || sp[0].Shrink {
		t.Errorf("shrink: %+v, %v", sp, err)
	}
	fat := *small
	fat.Partitions = append([]LayoutPartition(nil), small.Partitions...)
	fat.Partitions[0].Length -= mib
	if _, err := restorePlan(img, RestoreOptions{Layout: &fat}); !errors.Is(err, ErrShrinkUnsupported) {
		t.Errorf("shrinking the EFI partition: %v", err)
	}
	if _, err := restorePlan(img, RestoreOptions{Layout: l, Partitions: []int{3}}); err == nil {
		t.Error("layout and partition list together accepted")
	}
	other := *l
	other.Style = StyleMBR
	if _, err := restorePlan(img, RestoreOptions{Layout: &other}); err == nil {
		t.Error("style conversion accepted")
	}
}

func TestBlockOffset(t *testing.T) {
	w := partitionWrite{Src: &repo.PartitionImage{Offset: mib, Length: 10 * mib, BlockSize: uint32(mib)}, Offset: 5 * mib, Length: 4 * mib}
	if off, ok := w.blockOffset(2); !ok || off != 7*mib {
		t.Errorf("block 2: %d %v", off, ok)
	}
	if _, ok := w.blockOffset(4); ok {
		t.Error("block past the end of the target partition written")
	}
}

func ntfsBoot(hidden uint32, total uint64) []byte {
	b := bootSector(512, 8, total)
	binary.LittleEndian.PutUint32(b[0x1C:], hidden)
	b[510], b[511] = 0x55, 0xAA
	return b
}

func TestPatchHiddenSectors(t *testing.T) {
	b := ntfsBoot(2048, 1000)
	if !patchHiddenSectors(b, 5*mib, 512) {
		t.Fatal("not patched")
	}
	if got := binary.LittleEndian.Uint32(b[0x1C:]); got != 10240 {
		t.Errorf("hidden sectors %d", got)
	}
	if patchHiddenSectors(b, 5*mib, 512) {
		t.Error("unchanged sector reported as patched")
	}
	if patchHiddenSectors(make([]byte, 512), mib, 512) {
		t.Error("non-boot sector patched")
	}
	fat := make([]byte, 512)
	copy(fat[0x52:], "FAT32   ")
	fat[510], fat[511] = 0x55, 0xAA
	binary.LittleEndian.PutUint16(fat[0x32:], 6)
	if !patchHiddenSectors(fat, mib, 512) || bootSectorKind(fat) != "fat32" {
		t.Error("FAT32 not patched")
	}
	if got := backupBootSectors(fat, 512); len(got) != 1 || got[0] != 6*512 {
		t.Errorf("FAT32 backup %v", got)
	}
	if got := backupBootSectors(ntfsBoot(0, 1000), 512); len(got) != 1 || got[0] != 1000*512 {
		t.Errorf("NTFS backup %v", got)
	}
}
