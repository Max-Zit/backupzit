package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// #61 finding 1: image-layout says why a partition that can grow keeps its
// size as its minimum (e.g. not shut down cleanly), not only "yes".
func TestPrintImageLayoutShowsTheReason(t *testing.T) {
	img := &repo.DiskImage{Number: 0, Size: 64 << 30, Style: imaging.StyleGPT, Partitions: []repo.PartitionImage{
		{Number: 1, Length: 100 << 20},
		{Number: 3, Length: 58 << 30},
		{Number: 4, Length: 1 << 30},
	}}
	why := imaging.NoShrinkPrefix + "the file system in the backup was not shut down cleanly"
	limits := map[int]imaging.PartitionLimits{
		1: {Reason: "EFI System partitions keep their size", Why: imaging.ReasonFixedSize},
		3: {Resizable: true, MinSize: 58 << 30, UsedBytes: 13 << 30, Reason: why, Why: imaging.ReasonNoShrink},
		4: {Resizable: true, MinSize: 200 << 20},
	}
	var b bytes.Buffer
	printImageLayout(&b, img, limits, nil, 64<<30, 60<<30)
	lines := strings.Split(b.String(), "\n")
	row := func(n string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, "  "+n+"  ") {
				return l
			}
		}
		t.Fatalf("no row for partition %s in\n%s", n, b.String())
		return ""
	}
	if r := row("1"); !strings.HasSuffix(r, "no: EFI System partitions keep their size") {
		t.Errorf("fixed size: %q", r)
	}
	if r := row("3"); !strings.HasSuffix(r, "yes, but "+why) {
		t.Errorf("held up: %q", r)
	}
	if r := row("4"); !strings.HasSuffix(r, "yes") {
		t.Errorf("resizable: %q", r)
	}
}

// #61 finding 2 and the small things: the warning before a restore next to
// the backed up disk names it and the way out; a disk without a partition
// table is not listed as RAW or MBR.
func TestImageRestoreTexts(t *testing.T) {
	w := sourceDiskWarning(2)
	for _, s := range []string{"warning: disk 2 ", "same disk ID", "offline", "--keep-offline", "remove the old disk", "Disk Management"} {
		if !strings.Contains(w, s) {
			t.Errorf("warning %q does not say %q", w, s)
		}
	}
	for style, want := range map[string]string{imaging.StyleRaw: "not initialized", imaging.StyleGPT: "GPT", imaging.StyleMBR: "MBR"} {
		if got := diskStyleText(style); got != want {
			t.Errorf("%s: %q, want %q", style, got, want)
		}
	}
}
