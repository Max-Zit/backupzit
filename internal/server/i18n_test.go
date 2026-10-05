package server_test

import (
	"net/url"
	"strings"
	"testing"
)

// The language switch remembers the choice; pages, statuses and messages
// are then shown in Serbian, and switching back restores English.
func TestLanguageSwitch(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, body := admin.do("GET", "/", nil)
	if !strings.Contains(body, `<html lang="en"`) || !strings.Contains(body, "Dashboard") {
		t.Fatal("English is not the default")
	}
	if _, loc, _ := admin.do("POST", "/lang", url.Values{"lang": {"sr"}, "back": {"/jobs"}}); loc != "/jobs" {
		t.Fatalf("redirect to %q", loc)
	}
	_, _, body = admin.do("GET", "/", nil)
	if !strings.Contains(body, `<html lang="sr"`) || !strings.Contains(body, "Kontrolna tabla") || !strings.Contains(body, "Poslovi bekapa") {
		t.Error("dashboard not in Serbian")
	}
	_, loc, _ := admin.do("POST", "/settings/updates", url.Values{"auto_agents": {"on"}})
	if _, _, body = admin.do("GET", loc, nil); !strings.Contains(body, "Podešavanja ažuriranja su sačuvana.") {
		t.Error("message not translated")
	}
	// A crafted back address does not leave the console.
	if _, loc, _ := admin.do("POST", "/lang", url.Values{"lang": {"en"}, "back": {"//evil.example"}}); loc != "/" {
		t.Errorf("redirect to %q", loc)
	}
	if _, _, body = admin.do("GET", "/", nil); !strings.Contains(body, "Dashboard") {
		t.Error("switching back to English failed")
	}
}
