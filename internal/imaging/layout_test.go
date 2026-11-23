package imaging

import (
	"errors"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

const (
	mib = uint64(1) << 20
	gib = uint64(1) << 30
)

// windowsGPT is the usual layout of a Windows installation on a 60 GiB
// disk: EFI, MSR, Windows, recovery at the end.
func windowsGPT() *repo.DiskImage {
	return &repo.DiskImage{Style: StyleGPT, SectorSize: 512, Size: 60 * gib, Partitions: []repo.PartitionImage{
		{Number: 1, Offset: 1 * mib, Length: 100 * mib, GPTType: GPTTypeEFI, Included: true, FileSystem: "FAT32"},
		{Number: 2, Offset: 101 * mib, Length: 16 * mib, GPTType: GPTTypeMSR},
		{Number: 3, Offset: 117 * mib, Length: 60*gib - 117*mib - 1*mib - 600*mib, GPTType: GPTTypeBasicData, Included: true, FileSystem: "NTFS"},
		{Number: 4, Offset: 60*gib - 1*mib - 600*mib, Length: 600 * mib, GPTType: GPTTypeRecovery, Included: true, FileSystem: "NTFS"},
	}}
}

func windowsLimits(min uint64) map[int]PartitionLimits {
	return map[int]PartitionLimits{3: {Resizable: true, MinSize: min}}
}

func checkLayout(t *testing.T, img *repo.DiskImage, l *Layout, lim map[int]PartitionLimits) {
	t.Helper()
	if err := l.Validate(img, lim); err != nil {
		t.Fatalf("layout does not validate: %v\n%+v", err, l.Partitions)
	}
}

func TestSuggestLayoutAsSource(t *testing.T) {
	img := windowsGPT()
	l, err := SuggestLayout(img, 60*gib, windowsLimits(0), LayoutOptions{Mode: LayoutAsSource})
	if err != nil {
		t.Fatal(err)
	}
	if l.Moved(img) {
		t.Error("as-source layout should not move anything")
	}
	if l.Partitions[1].Restore {
		t.Error("MSR was not backed up and must not be restored")
	}
	if _, err := SuggestLayout(img, 50*gib, windowsLimits(0), LayoutOptions{Mode: LayoutAsSource}); !errors.Is(err, ErrTargetTooSmall) {
		t.Errorf("as-source on a smaller disk: got %v, want too small", err)
	}
}

func TestSuggestLayoutGrow(t *testing.T) {
	img := windowsGPT()
	lim := windowsLimits(0)
	l, err := SuggestLayout(img, 120*gib, lim, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	checkLayout(t, img, l, lim)
	p := l.Partitions
	if p[0].Length != 100*mib || p[1].Length != 16*mib || p[3].Length != 600*mib {
		t.Errorf("fixed partitions changed size: %+v", p)
	}
	if p[2].Length < 118*gib {
		t.Errorf("Windows partition should take the new space, got %d MiB", p[2].Length/mib)
	}
	_, end := UsableRange(StyleGPT, 120*gib, 512)
	if gap := end - p[3].End(); gap >= LayoutAlign {
		t.Errorf("recovery partition should end the disk, %d bytes left", gap)
	}
}

func TestSuggestLayoutShrink(t *testing.T) {
	img := windowsGPT()
	lim := windowsLimits(20 * gib)
	l, err := SuggestLayout(img, 32*gib, lim, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	checkLayout(t, img, l, lim)
	if got := l.Partitions[2].Length; got < 20*gib || got > 32*gib {
		t.Errorf("Windows partition: %d MiB", got/mib)
	}
	// Below the minimum size.
	_, err = SuggestLayout(img, 16*gib, lim, LayoutOptions{})
	var tse *TooSmallError
	if !errors.As(err, &tse) {
		t.Fatalf("got %v, want TooSmallError", err)
	}
	if tse.Needed < 20*gib+717*mib || tse.Needed > 21*gib {
		t.Errorf("needed %d MiB", tse.Needed/mib)
	}
	n, err := NeededDiskSize(img, lim, LayoutOptions{})
	if err != nil || n != tse.Needed {
		t.Errorf("NeededDiskSize = %d, %v; want %d", n, err, tse.Needed)
	}
	if l, err := SuggestLayout(img, n, lim, LayoutOptions{}); err != nil {
		t.Errorf("layout on a disk of exactly the needed size: %v", err)
	} else {
		checkLayout(t, img, l, lim)
	}
}

func TestSuggestLayoutNotResizable(t *testing.T) {
	img := windowsGPT()
	// BitLocker read raw: no limits, partition can only be moved.
	if _, err := SuggestLayout(img, 50*gib, nil, LayoutOptions{}); !errors.Is(err, ErrTargetTooSmall) {
		t.Errorf("got %v, want too small", err)
	}
	l, err := SuggestLayout(img, 100*gib, nil, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	checkLayout(t, img, l, nil)
	if l.Partitions[2].Length != img.Partitions[2].Length {
		t.Error("partition without limits must keep its size")
	}
	if err := l.Resize(3, 70*gib); err != nil {
		t.Fatal(err)
	}
	if err := l.Validate(img, map[int]PartitionLimits{3: {Reason: "BitLocker"}}); err == nil {
		t.Error("resizing a non-resizable partition must not validate")
	}
	// A partition that is not restored is created empty: any size will do.
	l.Partitions[2].Restore = false
	if err := l.Validate(img, map[int]PartitionLimits{3: {Reason: "BitLocker"}}); err != nil {
		t.Errorf("resizing a partition that is not restored: %v", err)
	}
}

func TestSuggestLayoutProportional(t *testing.T) {
	img := &repo.DiskImage{Style: StyleGPT, SectorSize: 512, Partitions: []repo.PartitionImage{
		{Number: 1, Offset: 1 * mib, Length: 100 * mib, GPTType: GPTTypeEFI, Included: true},
		{Number: 2, Offset: 101 * mib, Length: 40 * gib, GPTType: GPTTypeBasicData, Included: true, FileSystem: "NTFS"},
		{Number: 3, Offset: 101*mib + 40*gib, Length: 60 * gib, GPTType: GPTTypeBasicData, Included: true, FileSystem: "NTFS"},
		{Number: 4, Offset: 101*mib + 100*gib, Length: 1 * gib, GPTType: GPTTypeBasicData, Included: true, FileSystem: "NTFS"},
	}}
	lim := map[int]PartitionLimits{2: {Resizable: true, MinSize: 5 * gib}, 3: {Resizable: true, MinSize: 5 * gib}, 4: {Resizable: true}}
	l, err := SuggestLayout(img, 200*gib, lim, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	checkLayout(t, img, l, lim)
	a, b := float64(l.Partitions[1].Length), float64(l.Partitions[2].Length)
	if r := a / b; r < 0.65 || r > 0.68 {
		t.Errorf("partitions 2 and 3 should grow in proportion 40:60, got %.3f", r)
	}
	if l.Partitions[3].Length != 1*gib {
		t.Error("the small data partition should keep its size")
	}

	// One partition hits its minimum, the other takes the rest.
	lim[2] = PartitionLimits{Resizable: true, MinSize: 38 * gib}
	l, err = SuggestLayout(img, 60*gib, lim, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	checkLayout(t, img, l, lim)
	if l.Partitions[1].Length != 38*gib {
		t.Errorf("partition 2 should be clamped to its minimum, got %d MiB", l.Partitions[1].Length/mib)
	}
}

func TestSuggestLayoutSelection(t *testing.T) {
	img := windowsGPT()
	lim := windowsLimits(10 * gib)
	l, err := SuggestLayout(img, 60*gib, lim, LayoutOptions{Restore: []int{3}, Mode: LayoutAsSource})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range l.Partitions {
		if p.Restore != (p.Source == 3) {
			t.Errorf("partition %d restore=%v", p.Source, p.Restore)
		}
	}
	l, err = SuggestLayout(img, 30*gib, lim, LayoutOptions{Restore: []int{1, 3}, DropOthers: true})
	if err != nil {
		t.Fatal(err)
	}
	checkLayout(t, img, l, lim)
	if len(l.Partitions) != 2 || l.Partitions[1].Source != 3 {
		t.Errorf("dropped layout: %+v", l.Partitions)
	}
	if _, err := SuggestLayout(img, 60*gib, lim, LayoutOptions{Restore: []int{2}}); err == nil {
		t.Error("restoring a partition that was not backed up must fail")
	}
	if _, err := SuggestLayout(img, 60*gib, lim, LayoutOptions{Restore: []int{9}}); err == nil {
		t.Error("restoring a missing partition must fail")
	}
}

func TestLayoutMBR(t *testing.T) {
	img := &repo.DiskImage{Style: StyleMBR, SectorSize: 512, Partitions: []repo.PartitionImage{
		{Number: 1, Offset: 1 * mib, Length: 500 * mib, MBRType: 0x07, Bootable: true, Included: true, FileSystem: "NTFS"},
		{Number: 2, Offset: 501 * mib, Length: 59 * gib, MBRType: 0x07, Included: true, FileSystem: "NTFS"},
	}}
	lim := map[int]PartitionLimits{2: {Resizable: true, MinSize: 10 * gib}}
	l, err := SuggestLayout(img, 3*1024*gib, lim, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	checkLayout(t, img, l, lim)
	if l.Partitions[1].End() > uint64(mbrMaxSectors)*512 {
		t.Errorf("MBR partition ends beyond 2 TiB: %d", l.Partitions[1].End())
	}
	five := &repo.DiskImage{Style: StyleMBR, SectorSize: 512}
	for i := 0; i < 5; i++ {
		five.Partitions = append(five.Partitions, repo.PartitionImage{Number: i + 1, Offset: uint64(i+1) * gib, Length: gib, MBRType: 0x07})
	}
	l = &Layout{Style: StyleMBR, DiskSize: 10 * gib, SectorSize: 512}
	for _, p := range five.Partitions {
		l.Partitions = append(l.Partitions, LayoutPartition{Source: p.Number, Offset: p.Offset, Length: p.Length})
	}
	if err := l.Validate(five, nil); err == nil {
		t.Error("5 partitions on MBR must not validate")
	}
}

func TestLayoutResize(t *testing.T) {
	img := windowsGPT()
	lim := windowsLimits(20 * gib)
	l, err := SuggestLayout(img, 100*gib, lim, LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Resize(3, 50*gib+123); err != nil {
		t.Fatal(err)
	}
	checkLayout(t, img, l, lim)
	if l.Partitions[3].Offset != alignUp(l.Partitions[2].End(), LayoutAlign) {
		t.Error("recovery should follow the resized Windows partition")
	}
	if err := l.Resize(3, 10*gib); err != nil {
		t.Fatal(err)
	}
	if err := l.Validate(img, lim); err == nil {
		t.Error("resizing below the minimum size must not validate")
	}
	if err := l.Resize(7, gib); err == nil {
		t.Error("resizing an unknown partition must fail")
	}
}

func TestLayoutSectorSizeMismatch(t *testing.T) {
	img := windowsGPT()
	l, _ := SuggestLayout(img, 60*gib, nil, LayoutOptions{Mode: LayoutAsSource})
	l.SectorSize = 4096
	if err := l.Validate(img, nil); err == nil {
		t.Error("different sector size must not validate")
	}
}
