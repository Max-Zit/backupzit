package server

import "testing"

func TestEstimatePercent(t *testing.T) {
	for _, c := range []struct {
		files, prevFiles, scanned, prevBytes uint64
		want                                 int
		ok                                   bool
	}{
		{4, 10, 0, 0, 40, true},        // agent before 0.44: files only
		{4, 10, 800, 1000, 60, true},   // average of 40% and 80%
		{12, 10, 500, 1000, 75, true},  // more files than before: size still counts
		{12, 10, 1200, 1000, 0, false}, // both past the previous backup
		{0, 10, 0, 1000, 0, false},     // nothing yet
		{9, 10, 990, 1000, 94, true},   // average 94.5 → 94
		{10, 10, 999, 1000, 99, true},  // never 100 before the end
		{3, 0, 300, 0, 0, false},       // no previous backup
	} {
		got, ok := estimatePercent(c.files, c.prevFiles, c.scanned, c.prevBytes)
		if got != c.want || ok != c.ok {
			t.Errorf("%+v: got %d %v", c, got, ok)
		}
	}
}
