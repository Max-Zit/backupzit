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

func legacyMBR() *repo.DiskImage {
	return &repo.DiskImage{Style: StyleMBR, SectorSize: 512, MBRSignature: 0x1234, Partitions: []repo.PartitionImage{
		{Number: 1, Offset: mib, Length: 500 * mib, MBRType: 0x07, Bootable: true, Included: true, FileSystem: "NTFS"},
		{Number: 2, Offset: 501 * mib, Length: 59 * gib, MBRType: 0x07, Included: true, FileSystem: "NTFS"},
	}}
}

func TestConvertMBRToGPT(t *testing.T) {
	img := legacyMBR()
	lim := map[int]PartitionLimits{2: {Resizable: true, MinSize: 20 * gib}}
	l, err := SuggestLayout(img, 80*gib, lim, LayoutOptions{Style: StyleGPT})
	if err != nil {
		t.Fatal(err)
	}
	if l.Style != StyleGPT || len(l.Partitions) != 4 || l.Partitions[0].Source != SourceNewEFI || l.Partitions[1].Source != SourceNewMSR {
		t.Fatalf("layout: %+v", l)
	}
	if !l.Moved(img) {
		t.Error("a converted layout counts as moved")
	}
	plan, err := restorePlan(img, RestoreOptions{Layout: l})
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Create != SourceNewEFI || plan[0].Restore || plan[0].GPTType != GPTTypeEFI || plan[0].Length != NewEFISize {
		t.Errorf("EFI entry: %+v", plan[0])
	}
	if plan[3].GPTType != GPTTypeBasicData || plan[3].GPTID == "" || plan[3].Name != "Basic data partition" || !plan[3].Restore {
		t.Errorf("Windows entry: %+v", plan[3])
	}
	if plan[2].GPTID == plan[3].GPTID {
		t.Error("partition IDs not unique")
	}
	if _, err := SuggestLayout(img, 80*gib, lim, LayoutOptions{Style: StyleGPT, Mode: LayoutAsSource}); err == nil {
		t.Error("conversion with the source layout accepted")
	}
}

func TestConvertGPTToMBR(t *testing.T) {
	img := windowsGPT()
	lim := windowsLimits(10 * gib)
	l, err := SuggestLayout(img, 60*gib, lim, LayoutOptions{Style: StyleMBR})
	if err != nil {
		t.Fatal(err)
	}
	if l.Style != StyleMBR || len(l.Partitions) != 2 {
		t.Fatalf("EFI and MSR should be left out: %+v", l.Partitions)
	}
	if !l.Partitions[0].Active || l.Partitions[0].Source != 3 || l.Partitions[1].Active {
		t.Errorf("the Windows partition should be active: %+v", l.Partitions)
	}
	plan, err := restorePlan(img, RestoreOptions{Layout: l})
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].MBRType != 0x07 || !plan[0].Bootable || plan[1].MBRType != 0x27 {
		t.Errorf("MBR entries: %+v", plan)
	}
	if _, err := SuggestLayout(img, 3*1024*gib, lim, LayoutOptions{Style: StyleMBR}); err != nil {
		t.Errorf("large disk: %v", err)
	}
	many := windowsGPT()
	for i := 5; i <= 8; i++ {
		many.Partitions = append(many.Partitions, repo.PartitionImage{Number: i, Offset: uint64(i) * 70 * gib, Length: gib, GPTType: GPTTypeBasicData, Included: true, FileSystem: "NTFS"})
	}
	if _, err := SuggestLayout(many, 1024*gib, lim, LayoutOptions{Style: StyleMBR}); err == nil {
		t.Error("6 partitions on MBR accepted")
	}
}

func TestNewGUID(t *testing.T) {
	a, err := NewGUID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewGUID()
	if a == b || len(a) != 36 || a[14] != '4' {
		t.Errorf("GUIDs %s %s", a, b)
	}
	if _, err := guidBytesPortable(a); err != nil {
		t.Error(err)
	}
}

type recWriter struct{ at []int64 }

func (r *recWriter) WriteAt(b []byte, off int64) (int, error) {
	for i := range b {
		if b[i] != 0 {
			return 0, nil
		}
	}
	r.at = append(r.at, off, off+int64(len(b)))
	return len(b), nil
}

func TestClearEnds(t *testing.T) {
	var w recWriter
	if err := clearEnds(&w, 10<<20); err != nil {
		t.Fatal(err)
	}
	if len(w.at) != 4 || w.at[0] != 0 || w.at[1] != 1<<20 || w.at[2] != 9<<20 || w.at[3] != 10<<20 {
		t.Errorf("cleared %v", w.at)
	}
	w = recWriter{}
	_ = clearEnds(&w, 4096)
	if len(w.at) != 2 || w.at[1] != 4096 {
		t.Errorf("small partition cleared %v", w.at)
	}
	d := &Disk{Partitions: []Partition{{Offset: 1 << 20, Length: 100 << 20}}}
	if !samePlace(d, partitionWrite{Offset: 1 << 20, Length: 100 << 20}) || samePlace(d, partitionWrite{Offset: 1 << 20, Length: 50 << 20}) {
		t.Error("samePlace")
	}
}
