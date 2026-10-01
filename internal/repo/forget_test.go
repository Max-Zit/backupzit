package repo

import (
	"testing"
	"time"
)

func TestApplyPolicy(t *testing.T) {
	loc := time.UTC
	// Two backups a day (06:00 and 18:00) for 120 days, newest first.
	end := time.Date(2026, 10, 1, 18, 0, 0, 0, loc)
	var sns []*Snapshot
	for d := 0; d < 120; d++ {
		for _, h := range []int{0, 12} {
			sns = append(sns, &Snapshot{Time: end.AddDate(0, 0, -d).Add(-time.Duration(h) * time.Hour)})
		}
	}
	count := func(p RetentionPolicy) (int, []*Snapshot) {
		keep, remove := ApplyPolicy(sns, p, loc)
		if len(keep)+len(remove) != len(sns) {
			t.Fatalf("policy %v lost snapshots", p)
		}
		return len(keep), keep
	}

	if n, _ := count(RetentionPolicy{}); n != len(sns) {
		t.Errorf("empty policy must keep all, kept %d", n)
	}
	if n, keep := count(RetentionPolicy{KeepLast: 3}); n != 3 || !keep[0].Time.Equal(end) {
		t.Errorf("keep last 3: kept %d", n)
	}
	// 7 daily keeps the newest snapshot of each of the last 7 days (18:00).
	n, keep := count(RetentionPolicy{KeepDaily: 7})
	if n != 7 {
		t.Errorf("keep daily 7: kept %d", n)
	}
	for _, s := range keep {
		if s.Time.Hour() != 18 {
			t.Errorf("daily rule kept %v, want the newest of the day", s.Time)
		}
	}
	// Overlapping rules are a union: last 3 (two days) + 7 daily = 8.
	if n, _ := count(RetentionPolicy{KeepLast: 3, KeepDaily: 7}); n != 8 {
		t.Errorf("last 3 + daily 7: kept %d, want 8", n)
	}
	if n, _ := count(RetentionPolicy{KeepWeekly: 4}); n != 4 {
		t.Errorf("weekly 4: kept %d", n)
	}
	if n, _ := count(RetentionPolicy{KeepMonthly: 12}); n != 5 { // Jun..Oct 2026 only
		t.Errorf("monthly 12 over 120 days: kept %d, want 5", n)
	}
	if s := (RetentionPolicy{KeepLast: 3, KeepDaily: 7, KeepMonthly: 12}).String(); s != "keep 3 most recent, 7 daily and 12 monthly" {
		t.Errorf("String: %q", s)
	}
}
