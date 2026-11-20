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
// retention of a part deals only with its own backups; the connection's
// proxy also with those without a part and of proxies no longer there.
func TestOwnSnapshotParts(t *testing.T) {
	sn := func(tags ...string) *repo.Snapshot { return &repo.Snapshot{Tags: tags} }
	old := sn("vmware", "job:7")                // before the split
	mine := sn("vmware", "job:7", "proxy:100")  // the connection's proxy
	other := sn("vmware", "job:7", "proxy:200") // a further proxy
	gone := sn("vmware", "job:7", "proxy:300")  // a proxy that left
	foreign := sn("vmware", "job:8", "proxy:100")
	primary := api.Run{JobID: 7, Part: "proxy:100", PartPrimary: true, PartOthers: []string{"proxy:200"}}
	further := api.Run{JobID: 7, Part: "proxy:200"}
	whole := api.Run{JobID: 7}
	for _, c := range []struct {
		name string
		run  api.Run
		sn   *repo.Snapshot
		want bool
	}{
		{"primary old", primary, old, true}, {"primary own", primary, mine, true}, {"primary other", primary, other, false},
		{"primary gone", primary, gone, true}, {"primary foreign", primary, foreign, false},
		{"further old", further, old, false}, {"further own", further, other, true}, {"further primary", further, mine, false},
		{"further gone", further, gone, false},
		{"unsplit all", whole, other, true}, {"unsplit old", whole, old, true}, {"unsplit foreign", whole, foreign, false},
	} {
		if got := ownSnapshot(c.sn, c.run); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
