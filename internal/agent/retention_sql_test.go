package agent

import (
	"testing"
	"time"

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
