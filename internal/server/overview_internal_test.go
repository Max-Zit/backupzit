package server

import (
	"testing"
	"time"
)

func TestCoverageSummary(t *testing.T) {
	c := JobCoverage{Unit: "VM", Items: []string{"web"}}
	if got := c.Summary(); got != "1 VM: web" {
		t.Errorf("one: %q", got)
	}
	c.Items = []string{"a", "b", "c", "d", "e", "f", "g"}
	if got := c.Summary(); got != "7 VMs: a, b, c, d, e and 2 more" {
		t.Errorf("many: %q", got)
	}
	if (JobCoverage{}).Summary() != "" {
		t.Error("files job has a summary")
	}
}

func TestPreviousRun(t *testing.T) {
	// Every 2 hours 08-18 on weekdays; Sunday noon: the second-latest run
	// is Friday 14:00 (the last is 16:00).
	sc := Schedule{Kind: SchedInterval, EveryMinutes: 120, FromHour: 8, ToHour: 18, Days: []int{1, 2, 3, 4, 5}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local) // Sunday
	prev, ok := previousRun(sc, now, 2)
	if !ok || prev.Weekday() != time.Friday || prev.Hour() != 14 {
		t.Errorf("previous run: %v %v", prev, ok)
	}
	daily := Schedule{Kind: SchedDaily, Times: []string{"22:00"}}
	if prev, ok := previousRun(daily, now, 1); !ok || prev.Day() != 3 || prev.Hour() != 22 {
		t.Errorf("daily: %v", prev)
	}
}
