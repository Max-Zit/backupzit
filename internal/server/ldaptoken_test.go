package server_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/max-zit/backupzit/internal/server"
)

// B8: the API token of an LDAP user follows the directory: a lower role
// there lowers the token's, a removed account or group refuses it, and an
// unreachable directory is bridged by the last answer.
func TestLDAPTokenFollowsDirectory(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	if err := e.store.SetSetting(ctx, "ldap", server.LDAPSettings{Enabled: true, URL: "ldaps://dir.example:636"}); err != nil {
		t.Fatal(err)
	}
	var uid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO users(username, password_hash, role, source) VALUES('ana', '', 'operator', 'ldap') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	u, err := e.store.GetUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := e.store.CreateAPIToken(ctx, u, "monitoring", "operator", 30)
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) int {
		req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	answer := struct {
		role string
		err  error
	}{"operator", nil}
	defer server.UseLDAPLookup(func(string) (string, error) { return answer.role, answer.err })()

	if c := call("/api/v1/jobs"); c != 200 {
		t.Fatalf("operator in the directory: %d", c)
	}
	// Viewer in the directory now: read still works, starting a job not.
	answer.role = "viewer"
	e.srv.ForgetLDAPTokenChecks()
	if c := call("/api/v1/jobs"); c != 200 {
		t.Errorf("viewer read: %d", c)
	}
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/jobs/999/run", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := e.ts.Client().Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer started a job: %v %v", resp.StatusCode, err)
	}
	// The directory is down: the last answer counts.
	answer.err = errors.New("connect: timeout")
	e.srv.ForgetLDAPTokenChecks()
	if c := call("/api/v1/jobs"); c != http.StatusUnauthorized {
		// no answer cached after Forget: refused
		t.Errorf("unreachable directory without a cached answer: %d", c)
	}
	// Account removed from the directory.
	answer.role, answer.err = "", server.ErrLDAPNoAccount
	e.srv.ForgetLDAPTokenChecks()
	if c := call("/api/v1/jobs"); c != http.StatusUnauthorized {
		t.Errorf("removed account: %d", c)
	}
	answer.role, answer.err = "", server.ErrLDAPNoRole
	e.srv.ForgetLDAPTokenChecks()
	if c := call("/api/v1/jobs"); c != http.StatusUnauthorized {
		t.Errorf("no group: %d", c)
	}
}
