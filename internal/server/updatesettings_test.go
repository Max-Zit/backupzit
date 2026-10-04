package server_test

import (
	"net/url"
	"strings"
	"testing"
)

// The Updates page shows the saved settings (defaults: official source,
// notifications and agent updates on), so saving again keeps them.
func TestUpdateSettingsPage(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, body := admin.do("GET", "/settings/updates", nil)
	if !strings.Contains(body, `name="notify" checked`) || !strings.Contains(body, `name="auto_agents" checked`) ||
		!strings.Contains(body, `placeholder="https://github.com/Max-Zit/backupzit/releases/latest/download"`) {
		t.Error("defaults not shown")
	}
	src := t.TempDir()
	if _, loc, _ := admin.do("POST", "/settings/updates", url.Values{"source": {src}, "auto_agents": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("save: %s", loc)
	}
	_, _, body = admin.do("GET", "/settings/updates", nil)
	if !strings.Contains(body, `value="`+src+`"`) || strings.Contains(body, `name="notify" checked`) || !strings.Contains(body, `name="auto_agents" checked`) {
		t.Error("saved settings not shown")
	}
}
