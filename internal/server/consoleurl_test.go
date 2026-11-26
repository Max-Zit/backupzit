package server_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Without --public-url, links in notifications use the address users open
// the console with.
func TestConsoleURLFromBrowser(t *testing.T) {
	e := setup(t)
	e.srv.PublicURL = ""
	if u := e.srv.ConsoleURL(); u != "" {
		t.Fatalf("console URL before any visit: %q", u)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	admin.do("GET", "/", nil)
	if u := e.srv.ConsoleURL(); u != e.ts.URL {
		t.Errorf("console URL %q, want %q", u, e.ts.URL)
	}

	// A5: a user without the settings permission cannot move it with a
	// forged Host header.
	if _, loc, _ := admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {"view1"}, "role": {"viewer"},
		"password": {"correct-horse-9"}, "password2": {"correct-horse-9"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("create viewer: %s", loc)
	}
	viewer := newClient(t, e)
	if !viewer.login("view1", "correct-horse-9") {
		t.Fatal("viewer login")
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/", nil)
	req.Host = "evil.example"
	for _, ck := range viewer.c.Jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	resp, err := viewer.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer page with another Host: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if u := e.srv.ConsoleURL(); u != e.ts.URL {
		t.Errorf("viewer moved the console URL to %q", u)
	}

	e.srv.PublicURL = "https://backup.example"
	if u := e.srv.ConsoleURL(); u != "https://backup.example" {
		t.Errorf("public URL not preferred: %q", u)
	}
}
