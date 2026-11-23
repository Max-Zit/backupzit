package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/max-zit/backupzit/internal/imaging"
)

// lang is "de" or "en". The recovery program is German and English; the
// texts sit next to each other in the code (t("Deutsch", "English")).
var lang = detectLang()

func detectLang() string {
	for _, a := range os.Args[1:] {
		if a == "--lang=en" || a == "--en" {
			return "en"
		}
		if a == "--lang=de" || a == "--de" {
			return "de"
		}
	}
	if v := os.Getenv("BACKUPZIT_LANG"); v != "" {
		if strings.HasPrefix(strings.ToLower(v), "de") {
			return "de"
		}
		return "en"
	}
	return systemLang()
}

// t picks the text in the current language.
func t(de, en string) string {
	if lang == "de" {
		return de
	}
	return en
}

// tf is t with fmt.Sprintf.
func tf(de, en string, args ...any) string { return fmt.Sprintf(t(de, en), args...) }

// humanBytes formats a size in binary units, e.g. "39,7 GiB", the units
// of the size fields, so a size reads the same everywhere.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	s := fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
	if lang == "de" {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}

// reasonText says in the current language why a partition cannot be
// resized; kind is its type (imaging.Partition.Kind).
func reasonText(lim imaging.PartitionLimits, kind string) string {
	switch lim.Why {
	case imaging.ReasonNotBackedUp:
		return t("nicht gesichert", "not backed up")
	case imaging.ReasonFixedSize:
		return tf("Partitionen vom Typ „%s“ behalten ihre Größe", "%s partitions keep their size", kind)
	case imaging.ReasonBitLocker:
		return t("mit BitLocker verschlüsselt gesichert: nur verschiebbar, braucht nach dem Restore den Wiederherstellungsschlüssel",
			"backed up encrypted with BitLocker: can only be moved, needs its recovery key after the restore")
	case imaging.ReasonFileSystem:
		return t("Größenänderung nur für NTFS", "only NTFS is resized")
	case imaging.ReasonNotNTFS:
		return t("verschlüsselt (BitLocker) oder nicht als NTFS lesbar", "encrypted (BitLocker) or not readable as NTFS")
	case imaging.ReasonError:
		return t("nicht lesbar: ", "not readable: ") + lim.Reason
	}
	return lim.Reason
}

// span is a partition on disk in bytes; pix is its bar in pixels.
type span struct{ off, len uint64 }
type pix struct{ x, w int }

// barSpans places partition bars in width pixels: proportional to the
// disk, but each at least minW wide so small partitions (EFI, MSR) stay
// visible and labelled; the large ones give up the room. Free space keeps
// its proportional width.
func barSpans(parts []span, disk uint64, width, minW int) []pix {
	if disk == 0 || len(parts) == 0 {
		return nil
	}
	if n := len(parts); n*minW > width/2 {
		minW = max(1, width/(2*n))
	}
	scale := float64(width) / float64(disk)
	prop := make([]float64, len(parts))
	w := make([]float64, len(parts))
	var extra, slack float64
	for i, p := range parts {
		prop[i] = float64(p.len) * scale
		w[i] = max(prop[i], float64(minW))
		extra += w[i] - prop[i]
		if prop[i] > float64(minW) {
			slack += prop[i] - float64(minW)
		}
	}
	if extra > 0 && slack > 0 {
		f := min(1, extra/slack)
		for i := range w {
			if prop[i] > float64(minW) {
				w[i] -= (prop[i] - float64(minW)) * f
			}
		}
	}
	out := make([]pix, len(parts))
	var cursor float64
	var prevEnd uint64
	for i, p := range parts {
		if p.off > prevEnd {
			cursor += float64(p.off-prevEnd) * scale
		}
		out[i] = pix{x: int(cursor), w: int(w[i])}
		cursor += w[i]
		prevEnd = max(prevEnd, p.off+p.len)
	}
	return out
}

// gib converts bytes to GiB for size fields, and back.
func gib(b uint64) float64     { return float64(b) / (1 << 30) }
func fromGiB(g float64) uint64 { return uint64(g * (1 << 30)) }

// chooseBackupDir picks the folder for the boot data saved before a
// repair: ackupzit on the first root that is not on the disk being
// repaired (avoid) and can be written (not the CD the recovery medium
// may be), else the temp folder.
func chooseBackupDir(roots []string, avoid map[string]bool, writable func(dir string) bool) string {
	for _, r := range roots {
		if avoid[r] {
			continue
		}
		d := filepath.Join(r, "backupzit")
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && writable(d) {
			return d
		}
	}
	return os.TempDir()
}

// writableDir tells whether a file can be created in dir.
func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".bz-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// logText shows a progress line of the image functions (English, fixed
// beginnings) in the current language. The list of disk controller drivers
// is shortened to their number; unknown lines stay as they are.
func logText(s string) string {
	if rest, ok := strings.CutPrefix(s, "disk controller drivers set to start at boot: "); ok {
		n := len(strings.Fields(rest))
		return tf("Treiber für %d Disk-Controller beim Start aktiviert", "disk controller drivers enabled at start: %d", n)
	}
	if lang != "de" {
		return s
	}
	if rest, ok := strings.CutPrefix(s, "firmware "); ok {
		rest = strings.NewReplacer("Secure Boot on", "Secure Boot an", "Secure Boot off", "Secure Boot aus", ", boot files for ", ", Bootdateien für ").Replace(rest)
		return "Firmware " + rest
	}
	for _, p := range [][2]string{
		{"boot data backed up to ", "Bootdaten gesichert nach "},
		{"boot files for ", "Bootdateien für "},
		{"Windows found on ", "Windows gefunden auf "},
		{"adding drivers from ", "Treiber werden hinzugefügt aus "},
	} {
		if rest, ok := strings.CutPrefix(s, p[0]); ok {
			return p[1] + rest
		}
	}
	if s == "MBR boot code written" {
		return "Windows-Bootcode in den MBR geschrieben"
	}
	var n int
	if _, err := fmt.Sscanf(s, "partition %d is now active", &n); err == nil {
		return fmt.Sprintf("Partition %d ist jetzt aktiv", n)
	}
	return s
}

// problemText explains the problem pnputil reports for a device; a missing
// driver gets words, the raw code stays for support.
func problemText(p string) string {
	if strings.Contains(strings.ToUpper(p), "0XC0000490") || strings.Contains(p, "CM_PROB_FAILED_INSTALL") || strings.Contains(p, "(28)") {
		return t("kein passender Treiber geladen", "no matching driver loaded") + " (" + p + ")"
	}
	return p
}
