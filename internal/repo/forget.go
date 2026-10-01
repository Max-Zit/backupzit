package repo

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// RetentionPolicy says which snapshots of one backup job to keep. A
// snapshot is kept if any rule selects it. Daily/weekly/monthly rules keep
// the newest snapshot of each of the most recent N days/weeks/months that
// have snapshots. The zero policy keeps everything.
type RetentionPolicy struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
}

// Empty reports whether the policy keeps everything.
func (p RetentionPolicy) Empty() bool {
	return p.KeepLast <= 0 && p.KeepDaily <= 0 && p.KeepWeekly <= 0 && p.KeepMonthly <= 0
}

func (p RetentionPolicy) String() string {
	if p.Empty() {
		return "keep all backups"
	}
	var s []string
	add := func(n int, what string) {
		if n > 0 {
			s = append(s, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(p.KeepLast, "most recent")
	add(p.KeepDaily, "daily")
	add(p.KeepWeekly, "weekly")
	add(p.KeepMonthly, "monthly")
	out := "keep " + s[0]
	for i := 1; i < len(s); i++ {
		if i == len(s)-1 {
			out += " and " + s[i]
		} else {
			out += ", " + s[i]
		}
	}
	return out
}

// ApplyPolicy splits snapshots into those to keep and those to remove.
// Times are bucketed in loc (the agent's local time).
func ApplyPolicy(sns []*Snapshot, p RetentionPolicy, loc *time.Location) (keep, remove []*Snapshot) {
	if p.Empty() {
		return sns, nil
	}
	sorted := append([]*Snapshot(nil), sns...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.After(sorted[j].Time) }) // newest first

	kept := map[*Snapshot]bool{}
	for i := 0; i < p.KeepLast && i < len(sorted); i++ {
		kept[sorted[i]] = true
	}
	bucketRule := func(n int, bucket func(time.Time) string) {
		seen := map[string]bool{}
		for _, sn := range sorted {
			if len(seen) >= n {
				return
			}
			b := bucket(sn.Time.In(loc))
			if !seen[b] {
				seen[b] = true
				kept[sn] = true
			}
		}
	}
	bucketRule(p.KeepDaily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucketRule(p.KeepWeekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-%02d", y, w) })
	bucketRule(p.KeepMonthly, func(t time.Time) string { return t.Format("2006-01") })

	for _, sn := range sorted {
		if kept[sn] {
			keep = append(keep, sn)
		} else {
			remove = append(remove, sn)
		}
	}
	return keep, remove
}

// RemoveSnapshot deletes a snapshot. Its data is freed by Prune.
func (r *Repository) RemoveSnapshot(ctx context.Context, id ID) error {
	return r.be.Remove(ctx, "snapshots/"+id.String())
}
