package server_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/server"
)

// backupzit-server --reset-two-factor USER: the way back for a last
// administrator who lost the authenticator and the recovery codes. It turns
// two-factor sign-in off, signs the user out and is audited.
func TestResetTwoFactorFromCommandLine(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {"ana"}, "role": {"admin"}, "password": {"correct-horse-9"}, "password2": {"correct-horse-9"}})

	ana := newClient(t, e)
	if !ana.login("ana", "correct-horse-9") {
		t.Fatal("login")
	}
	_, _, page := ana.do("GET", "/account/2fa", nil)
	m := regexp.MustCompile(`<code class="secret">([A-Z2-7 ]+)</code>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no secret on the setup page")
	}
	secret := strings.ReplaceAll(m[1], " ", "")
	if code, _, _ := ana.do("POST", "/account/2fa/enable", url.Values{"code": {server.TOTPCodeForTest(secret, time.Now())}, "password": {"correct-horse-9"}}); code != 200 {
		t.Fatalf("enable 2FA: %d", code)
	}

	if err := e.store.ResetTwoFactor(ctx, "nobody"); err == nil {
		t.Error("unknown user accepted")
	}
	if err := e.store.ResetTwoFactor(ctx, "ana"); err != nil {
		t.Fatal(err)
	}
	// Signed out everywhere.
	if code, loc, _ := ana.do("GET", "/", nil); code != http.StatusSeeOther || loc != "/login" {
		t.Errorf("old session still valid: %d %s", code, loc)
	}
	// The password alone signs in again.
	c := newClient(t, e)
	if _, loc, _ := c.do("POST", "/login", url.Values{"username": {"ana"}, "password": {"correct-horse-9"}}); loc != "/" {
		t.Errorf("password sign-in after reset leads to %q", loc)
	}
	entries, _ := e.store.ListAudit(ctx, 10)
	found := false
	for _, x := range entries {
		found = found || (x.Action == "user.2fa_reset" && strings.Contains(x.Detail, "ana") && strings.Contains(x.Detail, "command line"))
	}
	if !found {
		t.Error("reset not in the audit log")
	}
}
