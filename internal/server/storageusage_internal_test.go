package server

import (
	"testing"
	"time"
)

// Growth is per calendar day, also across a daylight saving change (a day
// of 25 hours).
func TestGrowthAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Belgrade")
	if err != nil {
		t.Skip(err)
	}
	var days []UsagePoint
	for i := 0; i < 14; i++ {
		days = append(days, UsagePoint{time.Date(2026, 10, 18+i, 0, 0, 0, 0, loc), uint64(i+1) << 30})
	}
	g, ok := growth(days, 30)
	if !ok || g != float64(1<<30) {
		t.Fatalf("growth %v %v, want exactly 1 GiB a day", g, ok)
	}
}
