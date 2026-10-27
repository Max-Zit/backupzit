package server_test

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/server"
)

// Deleting backups needs, besides the name of the computer, the password —
// or the current two-factor code when two-factor sign-in is on — so that a
// stolen session alone cannot delete them.
func TestDeleteBackupsConfirmsIdentity(t *testing.T) {
	x := newRemoveEnv(t, "test")
	del := func(c *client, v url.Values) string {
		v.Set("backups", "all")
		v.Set("confirm", x.host)
		_, loc, _ := c.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), v)
		return loc
	}
	if _, _, page := x.admin.do("GET", fmt.Sprintf("/agents/remove/%d", x.agentID), nil); !strings.Contains(page, `name="reauth_password"`) {
		t.Fatal("removal page does not ask for the password")
	}
	if loc := del(x.admin, url.Values{}); !strings.Contains(loc, "err=") || !strings.Contains(loc, "password") {
		t.Fatalf("deleted without the password: %s", loc)
	}
	if loc := del(x.admin, url.Values{"reauth_password": {"wrong-pass-1"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("deleted with a wrong password: %s", loc)
	}
	if !x.agentExists() || x.snapshots(t) != 3 {
		t.Fatal("backups deleted without confirmation")
	}

	// An administrator with two-factor sign-in confirms with a code.
	now := time.Unix(1_800_000_000, 0)
	server.SetClock(x.e.srv, func() time.Time { return now })
	x.admin.do("POST", "/users", url.Values{"username": {"ana"}, "role": {"admin"}, "password": {"correct-horse-9"}, "password2": {"correct-horse-9"}})
	ana := newClient(t, x.e)
	ana.login("ana", "correct-horse-9")
	_, _, page := ana.do("GET", "/account/2fa", nil)
	m := regexp.MustCompile(`<code class="secret">([A-Z2-7 ]+)</code>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no two-factor secret")
	}
	secret := strings.ReplaceAll(m[1], " ", "")
	if code, _, _ := ana.do("POST", "/account/2fa/enable", url.Values{"code": {server.TOTPCodeForTest(secret, now)}}); code != 200 {
		t.Fatalf("enable 2FA: %d", code)
	}
	now = now.Add(time.Minute) // a new code; the used one cannot be replayed
	if _, _, page := ana.do("GET", fmt.Sprintf("/agents/remove/%d", x.agentID), nil); !strings.Contains(page, `name="reauth_code"`) || strings.Contains(page, `name="reauth_password"`) {
		t.Fatal("removal page does not ask for the two-factor code")
	}
	if loc := del(ana, url.Values{"reauth_password": {"correct-horse-9"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("password accepted instead of the code: %s", loc)
	}
	if loc := del(ana, url.Values{"reauth_code": {"000000"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("wrong code accepted: %s", loc)
	}
	if loc := del(ana, url.Values{"reauth_code": {server.TOTPCodeForTest(secret, now)}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("delete with the code: %s", loc)
	}
	if x.agentExists() {
		t.Fatal("agent not removed")
	}
	x.waitSnapshots(t, -1)
}
