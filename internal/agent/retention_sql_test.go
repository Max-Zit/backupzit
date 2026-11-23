package agent

import (
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
)

func TestRetentionSQLLogs(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	var sns []*repo.Snapshot
	add := func(h int, kind string) *repo.Snapshot {
		sn := &repo.Snapshot{Time: base.Add(time.Duration(h) * time.Hour), SQL: &repo.SQLBackup{Kind: kind}}
		sn.ID[0] = byte(len(sns) + 1)
		sns = append(sns, sn)
		return sn
	}
	f1 := add(0, "full")
	l1 := add(1, "log")
	f2 := add(24, "full")
	l2 := add(25, "log")
	f3 := add(48, "full")
	l3 := add(49, "log")
	remove := retentionRemove(sns, repo.RetentionPolicy{KeepLast: 2})
	got := map[*repo.Snapshot]bool{}
	for _, sn := range remove {
		got[sn] = true
	}
	if len(remove) != 2 || !got[f1] || !got[l1] {
		t.Fatalf("removed %d: f1 %v l1 %v", len(remove), got[f1], got[l1])
	}
	_, _, _ = f2, l2, f3
	_ = l3
}

// TestOwnSnapshotParts: with a VMware backup split over several proxies,
// retention of a part deals only with its own backups; a run without a
// part with all of the job's.
func TestOwnSnapshotParts(t *testing.T) {
	sn := func(tags ...string) *repo.Snapshot { return &repo.Snapshot{Tags: tags} }
	old := sn("vmware", "job:7")                // before the split
	mine := sn("vmware", "job:7", "proxy:100")  // the connection's proxy
	other := sn("vmware", "job:7", "proxy:200") // a further proxy
	foreign := sn("vmware", "job:8", "proxy:100")
	primary := api.Run{JobID: 7, Part: "proxy:100"}
	further := api.Run{JobID: 7, Part: "proxy:200"}
	whole := api.Run{JobID: 7}
	for _, c := range []struct {
		name string
		run  api.Run
		sn   *repo.Snapshot
		want bool
	}{
		{"primary old", primary, old, false}, {"primary own", primary, mine, true}, {"primary other", primary, other, false},
		{"primary foreign", primary, foreign, false},
		{"further old", further, old, false}, {"further own", further, other, true}, {"further primary", further, mine, false},
		{"unsplit all", whole, other, true}, {"unsplit old", whole, old, true}, {"unsplit foreign", whole, foreign, false},
	} {
		if got := ownSnapshot(c.sn, c.run); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRetentionPerPart (A1): the policy applies to each part on its own.
// Keep 2 daily over two proxies that each back up daily keeps 2 days of
// both, never the newer days of one part in place of the other's.
func TestRetentionPerPart(t *testing.T) {
	base := time.Date(2026, 11, 1, 22, 0, 0, 0, time.Local)
	var sns []*repo.Snapshot
	for d := 0; d < 4; d++ {
		day := base.AddDate(0, 0, d)
		sns = append(sns, &repo.Snapshot{Time: day, Tags: []string{"job:7", "proxy:1"}},
			&repo.Snapshot{Time: day.Add(-time.Hour), Tags: []string{"job:7", "proxy:2"}})
	}
	removed := retentionRemove(sns, repo.RetentionPolicy{KeepDaily: 2})
	gone := map[string]int{}
	for _, sn := range removed {
		gone[partOf(sn)]++
	}
	if len(removed) != 4 || gone["proxy:1"] != 2 || gone["proxy:2"] != 2 {
		t.Fatalf("removed %d (%v), want 2 of each part", len(removed), gone)
	}
	// One part only (or none): as before.
	if r := retentionRemove([]*repo.Snapshot{sns[0], sns[2]}, repo.RetentionPolicy{KeepLast: 1}); len(r) != 1 {
		t.Errorf("unsplit: removed %d", len(r))
	}
}
