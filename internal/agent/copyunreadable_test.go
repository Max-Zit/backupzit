package agent

import (
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
)

// MED 11a: a copy that left out unreadable source backups ends with a
// warning naming them, and keeps the newest copy it made.
func TestCopyUnreadableIsAWarning(t *testing.T) {
	res := api.RunResult{Status: api.StatusSuccess, SnapshotID: "abc", Message: "Copied 2 backups (0 already copied); 1 MB new data uploaded"}
	noteUnreadable(&res, []string{"backup 1234abcd of 2026-11-01 03:00 not copied, it cannot be read: tree 5678: not found"})
	if res.Status != api.StatusWarning || res.SnapshotID != "abc" {
		t.Errorf("status %s, snapshot %q", res.Status, res.SnapshotID)
	}
	if !strings.Contains(res.Message, "1 backup of the source could not be read and was not copied") {
		t.Errorf("message %q", res.Message)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "1234abcd") {
		t.Errorf("errors %q", res.Errors)
	}
	noteUnreadable(&res, []string{"a", "b"})
	if !strings.Contains(res.Message, "2 backups of the source could not be read and were not copied") {
		t.Errorf("message %q", res.Message)
	}
}
