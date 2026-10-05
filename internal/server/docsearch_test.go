package server_test

import (
	"net/url"
	"strings"
	"testing"
)

func TestDocsSearch(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, body := admin.do("GET", "/docs/search?q="+url.QueryEscape("setup menu SSH"), nil)
	if !strings.Contains(body, `href="/docs/install#setup-menu"`) || !strings.Contains(body, "<mark>") {
		t.Errorf("setup menu section not found:\n%s", body)
	}
	// Markup in the query is escaped, not interpreted.
	_, _, body = admin.do("GET", "/docs/search?q="+url.QueryEscape(`<script>alert(1)</script>`), nil)
	if strings.Contains(body, "<script>alert(1)") {
		t.Error("query not escaped")
	}
	if _, _, body = admin.do("GET", "/docs/search?q=zzqqxx", nil); !strings.Contains(body, "Nothing in the guide") {
		t.Error("empty result not explained")
	}
	// The box is on every guide page.
	if _, _, body = admin.do("GET", "/docs/agents", nil); !strings.Contains(body, `action="/docs/search"`) {
		t.Error("search box missing on guide pages")
	}
}
