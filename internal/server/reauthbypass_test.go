package server_test

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// H-4 (0.55): a stolen session alone can no longer get past the identity
// check before backups are deleted: turning on two-factor sign-in needs the
// password, and the users page does not change the signed-in user's own
// password or two-factor sign-in. A password reset by an administrator also
// revokes the user's API tokens (M-21).
func TestReauthCannotBeBypassedWithASession(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	for _, u := range []string{"ana", "marko"} {
		if _, loc, _ := admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {u}, "role": {"admin"},
			"password": {"correct-horse-9"}, "password2": {"correct-horse-9"}}); !strings.Contains(loc, "msg=") {
			t.Fatalf("create %s: %s", u, loc)
		}
	}
	ana := newClient(t, e)
	ana.login("ana", "correct-horse-9")

	// (C9) Creating a user or setting another user's password needs the
	// password too: a stolen session could otherwise make an account it
	// knows the password of and delete backups from there.
	for _, v := range []url.Values{{}, {"reauth_password": {"wrong-pass-1"}}} {
		v.Set("username", "eve")
		v.Set("role", "admin")
		v.Set("password", "eve-knows-this-1")
		v.Set("password2", "eve-knows-this-1")
		if _, loc, _ := ana.do("POST", "/users", v); !strings.Contains(loc, "err=") {
			t.Errorf("user created without the password (%v): %s", v.Has("reauth_password"), loc)
		}
	}
	if _, _, page := ana.do("GET", "/users", nil); !strings.Contains(page, `name="reauth_password"`) {
		t.Error("the new user form does not ask for the password")
	}
	var eve int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM users WHERE username='eve'`).Scan(&eve)
	if eve != 0 {
		t.Error("eve exists")
	}

	// (a) Turning on two-factor sign-in asks for the password.
	_, _, page := ana.do("GET", "/account/2fa", nil)
	if !strings.Contains(page, `name="password"`) {
		t.Error("the two-factor setup does not ask for the password")
	}
	for _, v := range []url.Values{{"code": {"123456"}}, {"code": {"123456"}, "password": {"wrong-pass-1"}}} {
		if _, loc, _ := ana.do("POST", "/account/2fa/enable", v); !strings.Contains(loc, "err=") || !strings.Contains(loc, "password") {
			t.Errorf("two-factor setup without the right password: %s", loc)
		}
	}

	// (b) and (c): the users page refuses the own password and own 2FA reset.
	var anaID int64
	users, _ := e.store.ListUsers(e.ctx)
	for _, u := range users {
		if u.Username == "ana" {
			anaID = u.ID
		}
	}
	if _, _, page := ana.do("GET", fmt.Sprintf("/users/%d", anaID), nil); strings.Contains(page, fmt.Sprintf(`action="/users/%d/password"`, anaID)) {
		t.Error("own user page offers setting the password without the current one")
	}
	if _, loc, _ := ana.do("POST", fmt.Sprintf("/users/%d/password", anaID), url.Values{"password": {"taken-over-77"}, "password2": {"taken-over-77"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("own password set through the users page: %s", loc)
	}
	if !ana.login("ana", "correct-horse-9") {
		t.Fatal("the own password was changed")
	}
	if _, loc, _ := ana.do("POST", fmt.Sprintf("/users/%d/2fa-reset", anaID), url.Values{}); !strings.Contains(loc, "err=") {
		t.Errorf("own two-factor reset through the users page: %s", loc)
	}

	// M-21: an administrator's password reset revokes the API tokens.
	code, _, body := ana.do("POST", "/account/tokens", url.Values{"name": {"tool"}, "role": {"viewer"}, "days": {"30"}})
	m := regexp.MustCompile(`<pre id="newtok">(bzt_[A-Za-z0-9_-]+)</pre>`).FindStringSubmatch(body)
	if code != 200 || m == nil {
		t.Fatalf("create token: %d", code)
	}
	status := func() int {
		req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/status", nil)
		req.Header.Set("Authorization", "Bearer "+m[1])
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := status(); c != 200 {
		t.Fatalf("token does not work: %d", c)
	}
	marko := newClient(t, e)
	marko.login("marko", "correct-horse-9")
	if _, loc, _ := marko.do("POST", fmt.Sprintf("/users/%d/password", anaID), url.Values{"password": {"new-horse-99"}, "password2": {"new-horse-99"}}); !strings.Contains(loc, "err=") {
		t.Errorf("another user's password set without confirming: %s", loc)
	}
	if _, loc, _ := marko.do("POST", fmt.Sprintf("/users/%d/password", anaID), url.Values{"reauth_password": {"correct-horse-9"}, "password": {"new-horse-99"}, "password2": {"new-horse-99"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("reset ana's password: %s", loc)
	}
	if c := status(); c != 401 {
		t.Errorf("token still works after the password reset: %d", c)
	}

	// M-20: wrong codes on the account pages count like failed sign-ins.
	ana.login("ana", "new-horse-99")
	blocked := false
	for i := 0; i < 30 && !blocked; i++ {
		_, loc, _ := ana.do("POST", "/account/2fa/enable", url.Values{"code": {"123456"}, "password": {"wrong-pass-1"}})
		blocked = strings.Contains(loc, "Too+many")
	}
	if !blocked {
		t.Error("no attempt limit on the password check of the two-factor setup")
	}
}
