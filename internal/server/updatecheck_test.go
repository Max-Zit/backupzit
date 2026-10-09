package server_test

import (
	"strings"
	"testing"
	"time"
)

// The last release check survives a restart of the console (an update
// restarts it), so the Updates page does not say "never" afterwards.
func TestUpdateCheckKeptAcrossRestart(t *testing.T) {
	e := setup(t)
	last := map[string]any{"time": time.Now().Add(-2 * time.Hour), "err": "source unreachable"}
	if err := e.store.SetSetting(e.ctx, "update_check", last); err != nil {
		t.Fatal(err)
	}
	e.srv.StartUpdates(e.ctx, t.TempDir())
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, body := admin.do("GET", "/settings/updates", nil)
	i := strings.Index(body, "Last check")
	if i < 0 {
		t.Fatal("no last check on the Updates page")
	}
	row := body[i:min(len(body), i+400)]
	if strings.Contains(row, "never") || !strings.Contains(row, "source unreachable") {
		t.Errorf("last check not restored: %s", row)
	}
}
