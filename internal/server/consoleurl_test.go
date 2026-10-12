package server_test

import (
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
	e.srv.PublicURL = "https://backup.example"
	if u := e.srv.ConsoleURL(); u != "https://backup.example" {
		t.Errorf("public URL not preferred: %q", u)
	}
}
