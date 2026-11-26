package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

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

// A data check or repair reports data packs, not bytes: the pages show them
// as packs, with the percentage of packs read (#64). Backups keep bytes.
func TestProgressDataCheckInPacks(t *testing.T) {
	s := &Server{}
	s.live.set(7, []api.RunProgress{{RunID: 1, Done: 5300, Total: 7300}, {RunID: 2, Done: 5300, Total: 7300}, {RunID: 3, Done: 5300, Total: 7300}}, time.Now())
	runs := s.withProgress(context.Background(), []Run{
		{ID: 1, AgentID: 7, Status: "running", Kind: api.KindRepoCheck},
		{ID: 2, AgentID: 7, Status: "running", Kind: api.KindRepoRepair},
		{ID: 3, AgentID: 7, Status: "running", Kind: api.KindRestore},
	})
	if err := s.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	tpl, ok := s.page("en", "run")
	if !ok {
		t.Fatal("no run page")
	}
	for i, want := range []string{"Data packs checked: 5300 / 7300", "Data packs checked: 5300 / 7300", "5.2 KiB / 7.1 KiB"} {
		p := runs[i].Progress
		if p == nil || p.Percent != 72 {
			t.Fatalf("run %d: %+v", i+1, p)
		}
		var b strings.Builder
		if err := tpl.ExecuteTemplate(&b, "runprog", p); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), want) || (i == 2) == strings.Contains(b.String(), "packs") {
			t.Errorf("run %d shows %q, want %q", i+1, b.String(), want)
		}
	}
}

// A copy shows its step, the backup, a first copy, the speed over the last
// minutes and the time left; when no data moves for copyStallAfter while
// the agent reports, a warning, and the run is handed once for its notice
// (#75).
func TestCopyProgressSpeedAndStall(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	now := t0
	s := &Server{clock: func() time.Time { return now }}
	if err := s.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	tpl, _ := s.page("en", "run")
	backup := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	report := func(done uint64) {
		s.live.set(7, []api.RunProgress{{RunID: 1, Done: done, Total: 6000 << 20, Stage: "copy", StageDone: 1, StageTotal: 3, Backup: &backup, First: true}}, now)
	}
	show := func() string {
		t.Helper()
		runs := s.withProgress(context.Background(), []Run{{ID: 1, AgentID: 7, Status: "running", Kind: api.KindCopy}})
		var b strings.Builder
		if err := tpl.ExecuteTemplate(&b, "runprog", runs[0].Progress); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	report(0)
	if got := show(); strings.Contains(got, "B/s") || strings.Contains(got, " left") {
		t.Errorf("a speed from one report: %s", got)
	}
	now = now.Add(time.Minute)
	report(600 << 20)
	got := show()
	for _, want := range []string{"Copying backup 1 of 3 (" + backup.Local().Format("2006-01-02 15:04") + ")", "first copy to this storage", "MiB/s", "about 9 min left"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in %s", want, got)
		}
	}
	if strings.Contains(got, "no data moved") {
		t.Error("a moving copy is shown as stalled")
	}
	now = now.Add(31 * time.Minute)
	report(600 << 20) // the agent still reports, nothing moves
	if got := show(); !strings.Contains(got, "no data moved for 31 min") {
		t.Errorf("no stall warning: %s", got)
	}
	if st := s.live.stalledCopies(now); len(st) != 1 || !st[1].Equal(t0.Add(time.Minute)) {
		t.Errorf("stalled copies %v", st)
	}
	if st := s.live.stalledCopies(now); len(st) != 0 {
		t.Errorf("a stalled run handed twice: %v", st)
	}
	now = now.Add(time.Minute)
	report(700 << 20)
	if got := show(); strings.Contains(got, "no data moved") {
		t.Errorf("still stalled after data moved: %s", got)
	}
	// The speeds of a finished copy.
	c := &CopyRunStats{Packed: 100 << 20, ReadSeconds: 10, BytesStored: 50 << 20, WriteSeconds: 0.5}
	if r, w := c.ReadSpeed(), c.WriteSpeed(); !strings.Contains(r, "10") || !strings.HasSuffix(r, "/s") || w != "" {
		t.Errorf("speeds %q %q", r, w)
	}
}
