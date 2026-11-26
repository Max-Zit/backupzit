package server_test

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var pendingForm = regexp.MustCompile(`action="([^"]+)" class="row">\s*<input type="hidden" name="pending" value="([^"]+)"`)

// confirmIdentity follows the page that shows a machine's fingerprints
// before the password is sent (B3): loc is the redirect of the form post,
// empty when the page was shown instead.
func confirmIdentity(t *testing.T, c *client, loc, body string) string {
	t.Helper()
	if loc != "" {
		return loc
	}
	m := pendingForm.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("neither a redirect nor the confirmation page: %.300s", body)
	}
	_, loc, _ = c.do("POST", m[1], url.Values{"pending": {m[2]}})
	return loc
}

// pendingPrints are the fingerprints a confirmation page shows.
func pendingPrints(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<td class="mono">([^<]+)</td>`).FindAllStringSubmatch(body, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}
