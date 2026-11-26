package server_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A20: requests from this machine with X-Forwarded-For while no reverse
// proxy is set show a warning on the security settings.
func TestUnsetProxyHint(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	const hint = "through a reverse proxy on this machine"
	if _, _, body := admin.do("GET", "/settings/security", nil); strings.Contains(body, hint) {
		t.Fatal("hint without a proxy")
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if resp, err := admin.c.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if _, _, body := admin.do("GET", "/settings/security", nil); !strings.Contains(body, hint) {
		t.Error("no hint after a proxied request")
	}
	// Once the proxy is set, the hint goes.
	if _, loc, _ := admin.do("POST", "/settings/login-protection", url.Values{"max_attempts": {"5"}, "window_minutes": {"15"}, "block_minutes": {"30"},
		"max_user_attempts": {"20"}, "trusted_proxies": {"127.0.0.1\n::1"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("save proxies: %s", loc)
	}
	if _, _, body := admin.do("GET", "/settings/security", nil); strings.Contains(body, hint) {
		t.Error("hint stays after setting the proxy")
	}
}
