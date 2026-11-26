// Package anomaly finds backups that look like ransomware or mass deletion
// by comparing them with the job's recent history. The console flags them
// (and pauses the job's retention); since 0.62 the agent runs the same
// check before it applies retention inside the run, so a suspicious backup
// cannot remove the oldest clean point first (B5).
package anomaly

import (
	"fmt"
	"sort"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
)

// MinHistory is how many earlier backups a comparison needs.
const MinHistory = 3

// Checked tells whether backups of kind are compared at all.
func Checked(kind string) bool {
	return kind == api.KindBackup || kind == api.KindImageBackup || kind == api.KindSystemBackup || kind == api.KindVMBackup
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64{}, v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

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
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Detect returns human-readable reasons why cur looks suspicious
// compared with hist (older backups of the same job, newest first).
func Detect(kind string, cur repo.SnapshotStats, hist []repo.SnapshotStats) []string {
	if len(hist) < MinHistory {
		return nil
	}
	var reasons []string
	const mb = 1 << 20
	if kind == api.KindBackup || kind == api.KindSystemBackup {
		changed := cur.FilesNew + cur.FilesChanged
		ratio := float64(changed) / float64(max(cur.Files, 1))
		var base []float64
		for _, h := range hist {
			base = append(base, float64(h.FilesNew+h.FilesChanged)/float64(max(h.Files, 1)))
		}
		if b := median(base); changed >= 100 && ratio >= 0.3 && ratio >= 5*b {
			reasons = append(reasons, fmt.Sprintf("%d files (%.0f %%) changed or new — usually %.1f %%", changed, 100*ratio, 100*b))
		}
		if prev := hist[0].Files; prev >= 100 && cur.Files < prev && float64(prev-cur.Files)/float64(prev) >= 0.3 {
			reasons = append(reasons, fmt.Sprintf("the number of files dropped from %d to %d", prev, cur.Files))
		}
	}
	if cur.BytesAdded >= 50*mb {
		var comp []float64
		for _, h := range hist {
			if h.BytesAdded >= 10*mb {
				comp = append(comp, float64(h.BytesStored)/float64(h.BytesAdded))
			}
		}
		c := float64(cur.BytesStored) / float64(cur.BytesAdded)
		if b := median(comp); len(comp) >= 2 && b <= 0.85 && c >= 0.97 {
			reasons = append(reasons, fmt.Sprintf("new data does not compress (%.0f %% vs. usually %.0f %%) — typical for encrypted files", 100*c, 100*b))
		}
	}
	var added []float64
	for _, h := range hist {
		added = append(added, float64(h.BytesAdded))
	}
	if b := median(added); cur.BytesAdded >= 1<<30 && float64(cur.BytesAdded) >= 10*b {
		reasons = append(reasons, fmt.Sprintf("%s of new data — usually %s", humanBytes(cur.BytesAdded), humanBytes(uint64(b))))
	}
	return reasons
}
