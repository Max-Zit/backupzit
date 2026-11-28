package server_test

import (
	"strings"
	"testing"
)

// Plan item I: the footer's "AGPLv3" links to the source code of the
// running version (its tag), and to the repository for a development build.
func TestFooterLinksSource(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	for v, want := range map[string]string{
		"dev":          `href="https://github.com/Max-Zit/backupzit" target="_blank" rel="noopener"`,
		"0.63.0":       `href="https://github.com/Max-Zit/backupzit/tree/v0.63.0" target="_blank" rel="noopener"`,
		"0.63.0-4-gab": `href="https://github.com/Max-Zit/backupzit" target="_blank" rel="noopener"`,
	} {
		e.srv.Version = v
		_, _, body := admin.do("GET", "/", nil)
		if !strings.Contains(body, `<a class="license-link" `+want) || !strings.Contains(body, ">AGPLv3</a>") {
			t.Errorf("version %s: no source link %s", v, want)
		}
	}
}
