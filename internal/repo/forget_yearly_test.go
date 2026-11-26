package repo

import (
	"testing"
	"time"
)

// #77: the yearly rule keeps the newest backup of each of the last N years
// that have backups; with the other rules, a backup is kept if any rule
// selects it.
func TestRetentionYearly(t *testing.T) {
	var sns []*Snapshot
	add := func(y int, m time.Month, d int) *Snapshot {
		sn := &Snapshot{Time: time.Date(y, m, d, 12, 0, 0, 0, time.UTC)}
		sn.ID[0], sn.ID[1] = byte(len(sns)), byte(y)
		sns = append(sns, sn)
		return sn
	}
	old := add(2019, 6, 1)
	y20a, y20b := add(2020, 1, 1), add(2020, 12, 31)
	y21 := add(2021, 7, 7)
	y23 := add(2023, 3, 3)
	newest := add(2026, 11, 26)
	keep, remove := ApplyPolicy(sns, RetentionPolicy{KeepLast: 1, KeepYearly: 3}, time.UTC)
	kept := map[*Snapshot]bool{}
	for _, sn := range keep {
		kept[sn] = true
	}
	// Years with backups, newest first: 2026, 2023, 2021 (3 yearly).
	for sn, want := range map[*Snapshot]bool{newest: true, y23: true, y21: true, y20b: false, y20a: false, old: false} {
		if kept[sn] != want {
			t.Errorf("%s kept %v, want %v", sn.Time.Format("2006-01-02"), kept[sn], want)
		}
	}
	if len(keep)+len(remove) != len(sns) {
		t.Errorf("keep %d + remove %d != %d", len(keep), len(remove), len(sns))
	}
	if s := (RetentionPolicy{KeepDaily: 7, KeepYearly: 10}).String(); s != "keep 7 daily and 10 yearly" {
		t.Errorf("String: %q", s)
	}
	if (RetentionPolicy{KeepYearly: 1}).Empty() {
		t.Error("a yearly-only policy counts as empty")
	}
}
