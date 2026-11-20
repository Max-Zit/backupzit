package server_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/server"
)

// The "new version available" banner: administrators see it on every page
// except the Updates page itself; other roles never see it. The × hides it
// for that user for five days, also on other browsers; a newer version
// shows it again at once.
func TestUpdateBanner(t *testing.T) {
	e := setup(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	server.SetClock(e.srv, func() time.Time { return now })
	e.srv.Version = "0.51.0"
	e.srv.StartUpdates(e.ctx, t.TempDir())

	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {"vera"}, "role": {"viewer"}, "password": {"correct-horse-9"}, "password2": {"correct-horse-9"}})
	viewer := newClient(t, e)
	if !viewer.login("vera", "correct-horse-9") {
		t.Fatal("viewer login")
	}
	const hide = `action="/settings/updates/hide-banner"`
	shown := func(c *client, path string) bool {
		t.Helper()
		_, _, body := c.do("GET", path, nil)
		return strings.Contains(body, hide)
	}

	if shown(admin, "/") {
		t.Fatal("banner without a newer release")
	}
	e.srv.SetAvailableUpdate("0.51.0") // not newer
	if shown(admin, "/") {
		t.Fatal("banner for the installed version")
	}

	e.srv.SetAvailableUpdate("0.52.0")
	_, _, body := admin.do("GET", "/jobs", nil)
	for _, want := range []string{hide, "BackupZit 0.52.0 is available (installed: 0.51.0).", `href="/settings/updates"`,
		`href="https://github.com/Max-Zit/backupzit/releases/tag/v0.52.0"`} {
		if !strings.Contains(body, want) {
			t.Errorf("banner lacks %q", want)
		}
	}
	if shown(admin, "/settings/updates") {
		t.Error("banner on the Updates page itself")
	}
	if shown(viewer, "/") {
		t.Error("viewer sees the banner")
	}
	if code, _, _ := viewer.do("POST", "/settings/updates/hide-banner", url.Values{}); code != http.StatusForbidden {
		t.Errorf("viewer may hide the banner: %d", code)
	}

	// × hides it for this user, in every browser.
	if code, loc, _ := admin.do("POST", "/settings/updates/hide-banner", url.Values{}); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("hide: %d %s", code, loc)
	}
	other := newClient(t, e)
	other.login("admin", "admin-pass-123")
	if shown(admin, "/") || shown(other, "/jobs") {
		t.Error("banner still shown after ×")
	}
	now = now.Add(5*24*time.Hour - time.Minute)
	if shown(admin, "/") {
		t.Error("banner back before five days")
	}
	now = now.Add(2 * time.Minute)
	if !shown(admin, "/") {
		t.Error("banner not back after five days")
	}

	// A newer version than the hidden one shows it again at once.
	admin.do("POST", "/settings/updates/hide-banner", url.Values{})
	if shown(admin, "/") {
		t.Fatal("banner shown right after ×")
	}
	e.srv.SetAvailableUpdate("0.53.0")
	if !shown(admin, "/") {
		t.Error("newer version does not show the banner again")
	}

	// A container console is updated by pulling the image: no Install button.
	t.Setenv("BACKUPZIT_CONTAINER", "1")
	_, _, body = admin.do("GET", "/", nil)
	if !strings.Contains(body, "Pull the new image.") || strings.Contains(body, `<a class="btn" href="/settings/updates">`) {
		t.Error("container banner should point to the image, without Install")
	}
}
