package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/imaging"
)

func withLang(t *testing.T, l string) {
	t.Helper()
	old := lang
	lang = l
	t.Cleanup(func() { lang = old })
}

func TestReasonTextIsTranslated(t *testing.T) {
	lim := imaging.PartitionLimits{Reason: "EFI System partitions keep their size", Why: imaging.ReasonFixedSize}
	withLang(t, "de")
	if got := reasonText(lim, "EFI System"); strings.Contains(got, "keep their size") || !strings.Contains(got, "EFI System") {
		t.Errorf("de: %q", got)
	}
	withLang(t, "en")
	if got := reasonText(lim, "EFI System"); !strings.Contains(got, "keep their size") {
		t.Errorf("en: %q", got)
	}
	// A read error has no translation: its text is shown.
	withLang(t, "de")
	if got := reasonText(imaging.PartitionLimits{Reason: "read block 7: EOF", Why: imaging.ReasonError}, ""); !strings.Contains(got, "read block 7: EOF") {
		t.Errorf("error: %q", got)
	}
}

func TestHumanBytesUsesTheUnitsOfTheSizeFields(t *testing.T) {
	withLang(t, "de")
	for _, c := range []struct {
		b    uint64
		want string
	}{
		{512, "512 B"},
		{272629760, "260,0 MiB"},
		{42658168832, "39,7 GiB"},
		{40 << 30, "40,0 GiB"},
		{2 << 40, "2,0 TiB"},
	} {
		if got := humanBytes(c.b); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}

func TestBarSpansKeepSmallPartitionsVisible(t *testing.T) {
	// The test disk: EFI 260 MiB, MSR 16 MiB, Windows 39.7 GiB on 40 GiB.
	parts := []span{{1 << 20, 272629760}, {273678336, 16777216}, {290455552, 42658168832}}
	got := barSpans(parts, 40<<30, 800, 60)
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i, g := range got {
		if g.w < 60 {
			t.Errorf("bar %d is %d px wide, want at least 60", i, g.w)
		}
		if i > 0 && g.x < got[i-1].x+got[i-1].w {
			t.Errorf("bar %d overlaps bar %d: %v", i, i-1, got)
		}
	}
	if end := got[2].x + got[2].w; end > 800 {
		t.Errorf("bars end at %d, wider than 800", end)
	}
	if got[2].w < 600 {
		t.Errorf("Windows should keep most of the width: %v", got)
	}
	// Free space at the end stays free.
	half := []span{{0, 20 << 30}}
	if g := barSpans(half, 40<<30, 800, 60); g[0].x != 0 || g[0].w != 400 {
		t.Errorf("half the disk: %v", g)
	}
}

func TestChooseBackupDir(t *testing.T) {
	cd, target, stick, other := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for _, r := range []string{cd, target, stick} {
		if err := os.Mkdir(filepath.Join(r, "backupzit"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	readOnly := filepath.Join(cd, "backupzit")
	writable := func(dir string) bool { return dir != readOnly }
	avoid := map[string]bool{target: true}

	// The CD is read-only and the target is the disk being repaired: the stick wins.
	if got := chooseBackupDir([]string{cd, target, stick, other}, avoid, writable); got != filepath.Join(stick, "backupzit") {
		t.Errorf("got %q", got)
	}
	// Nothing suitable: the temp folder.
	if got := chooseBackupDir([]string{cd, target, other}, avoid, writable); got != os.TempDir() {
		t.Errorf("fallback: got %q", got)
	}
}

func TestLogTextInGerman(t *testing.T) {
	withLang(t, "de")
	for in, want := range map[string]string{
		`boot data backed up to D:\backupzit\boot-backup-disk0-1`:                `Bootdaten gesichert nach D:\backupzit\boot-backup-disk0-1`,
		"disk controller drivers set to start at boot: storahci stornvme pciide": "Treiber für 3 Disk-Controller beim Start aktiviert",
		"partition 1 is now active":                                              "Partition 1 ist jetzt aktiv",
		"MBR boot code written":                                                  "Windows-Bootcode in den MBR geschrieben",
		"boot files for UEFI":                                                    "Bootdateien für UEFI",
		"firmware UEFI (Secure Boot on), boot files for UEFI":                    "Firmware UEFI (Secure Boot an), Bootdateien für UEFI",
		`Windows found on E:`:                                                    `Windows gefunden auf E:`,
		`new hardware: Windows found on E:`:                                      `Neue Hardware: Windows gefunden auf E:`,
		"wrote 3 partitions, 42947575808 bytes in 9m46s":                         "3 Partitionen geschrieben, 40,0 GiB in 9m46s",
		"verifying the snapshot":                                                 "Backup wird geprüft",
		"snapshot verified":                                                      "Backup geprüft, keine Fehler",
		"warning: rebuild the boot files: x":                                     "Hinweis: rebuild the boot files: x",
		"something new":                                                          "something new",
	} {
		if got := logText(in); got != want {
			t.Errorf("logText(%q) = %q, want %q", in, got, want)
		}
	}
	withLang(t, "en")
	if got := logText("disk controller drivers set to start at boot: a b"); got != "disk controller drivers enabled at start: 2" {
		t.Errorf("en: %q", got)
	}
}

func TestProblemText(t *testing.T) {
	withLang(t, "de")
	if got := problemText("0xC0000490"); !strings.Contains(got, "kein passender Treiber") || !strings.Contains(got, "0xC0000490") {
		t.Errorf("missing driver: %q", got)
	}
	if got := problemText("CM_PROB_FAILED_INSTALL (28)"); !strings.Contains(got, "kein passender Treiber") {
		t.Errorf("code 28: %q", got)
	}
	if got := problemText("0x0000000A"); got != "0x0000000A" {
		t.Errorf("other: %q", got)
	}
}
