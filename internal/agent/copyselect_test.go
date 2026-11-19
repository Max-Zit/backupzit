package agent

import (
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

// A copy job keeps fewer backups than its source: only those are copied,
// and what it copies is exactly what its retention keeps afterwards.
func TestCopySelectionFollowsTheCopyRetention(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.Local)
	var sns []*repo.Snapshot
	for i := 29; i >= 0; i-- { // 30 daily backups, oldest first
		sns = append(sns, &repo.Snapshot{Time: now.AddDate(0, 0, -i)})
	}
	for _, p := range []repo.RetentionPolicy{{KeepLast: 7}, {KeepDaily: 7, KeepWeekly: 4}, {KeepMonthly: 12}} {
		got, older := copySelection(sns, &p)
		if older == 0 || len(got)+older != len(sns) {
			t.Errorf("%v: %d to copy, %d left out", p, len(got), older)
		}
		// Retention in the copy after copying them removes none of them.
		if again := repo.RetentionRemove(got, p); len(again) != 0 {
			t.Errorf("%v: %d copied backups would be removed right away", p, len(again))
		}
		if got[len(got)-1] != sns[len(sns)-1] {
			t.Errorf("%v: the newest backup is not copied", p)
		}
	}
	if got, older := copySelection(sns, nil); len(got) != len(sns) || older != 0 {
		t.Error("without retention not everything is copied")
	}
	if got, older := copySelection(sns, &repo.RetentionPolicy{}); len(got) != len(sns) || older != 0 {
		t.Error("an empty policy left backups out")
	}
}
