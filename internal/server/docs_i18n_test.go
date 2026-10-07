package server_test

import (
	"net/url"
	"strings"
	"testing"
)

// Translated pages of the user guide are shown in their language, with
// translated titles in the navigation; untranslated pages stay English.
func TestDocsTranslated(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	admin.do("POST", "/lang", url.Values{"lang": {"sr"}, "back": {"/"}})
	_, _, body := admin.do("GET", "/docs/start", nil)
	if !strings.Contains(body, "<h1>Prvi koraci</h1>") || !strings.Contains(body, ">Instalacija servera</a>") || !strings.Contains(body, "<title>Uputstvo · Prvi koraci") {
		t.Errorf("Serbian start page not shown: %.80s / %.300s", body[strings.Index(body, "<html"):], body[strings.Index(body, "<h1"):])
	}
	_, _, body = admin.do("GET", "/docs/search?q=deduplikacija", nil)
	if !strings.Contains(body, "Snimci i deduplikacija") {
		t.Error("search does not find the Serbian text")
	}
	admin.do("POST", "/lang", url.Values{"lang": {"en"}, "back": {"/"}})
	if _, _, body = admin.do("GET", "/docs/start", nil); !strings.Contains(body, "<h1>Getting started</h1>") {
		t.Error("English page not shown")
	}
}
