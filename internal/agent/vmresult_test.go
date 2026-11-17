package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/hyperv"
	"github.com/max-zit/backupzit/internal/repo"
)

// A VM that could not be backed up fails the run and is named, and no
// retention runs, so its older restore points stay (issue #38). Warnings
// alone (a crash-consistent checkpoint) keep the run a warning.
func TestVMBackupResult(t *testing.T) {
	guests := func(n int) []repo.Guest { return make([]repo.Guest, n) }

	sn := &repo.Snapshot{Guests: guests(4), Stats: repo.SnapshotStats{
		Errors: []string{"vm-E: scsi0:0: upload pack fa2f: s3 put data/fa/...: timeout awaiting response headers", "vm-A: took a crash-consistent one"},
		Failed: []string{"vm-E"},
	}}
	res, retention := vmBackupResult(sn, nil)
	if res.Status != api.StatusFailed || retention || res.SnapshotID == "" {
		t.Fatalf("guest missing: %s retention=%v snapshot=%q", res.Status, retention, res.SnapshotID)
	}
	if !strings.Contains(res.Message, "No backup of vm-E") || !strings.Contains(res.Message, "other 4 guests") {
		t.Errorf("message %q", res.Message)
	}

	sn.Stats.Failed = nil
	sn.Stats.Errors = sn.Stats.Errors[1:]
	if res, retention := vmBackupResult(sn, nil); res.Status != api.StatusWarning || !retention {
		t.Errorf("warnings only: %s retention=%v", res.Status, retention)
	}

	sn.Stats.Errors = nil
	if res, retention := vmBackupResult(sn, nil); res.Status != api.StatusSuccess || !retention || res.Message != "Backed up 4 guests" {
		t.Errorf("all fine: %s %q retention=%v", res.Status, res.Message, retention)
	}

	// Application-consistent checkpoints required: the VMs without one are
	// named as well.
	sn.Stats.Failed = []string{"sql01"}
	err := fmt.Errorf("%w for 1 of 5 VMs; they were not backed up", hyperv.ErrNotAppConsistent)
	if res, retention := vmBackupResult(sn, err); res.Status != api.StatusFailed || retention || !strings.Contains(res.Message, "sql01") {
		t.Errorf("not app-consistent: %s %q retention=%v", res.Status, res.Message, retention)
	}

	if res, retention := vmBackupResult(nil, errors.New("no VM could be backed up")); res.Status != api.StatusFailed || retention {
		t.Errorf("nothing backed up: %s retention=%v", res.Status, retention)
	}
}
