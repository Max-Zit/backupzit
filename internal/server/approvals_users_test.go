package server_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// With four-eyes approval on, one administrator cannot get a second
// approver alone: new approvers, more rights, password and two-factor
// resets of approvers and LDAP changes wait for approval, accounts created
// after a request cannot decide it, and two administrators always remain.
func TestFourEyesNoSelfApprover(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	const pw = "correct-horse-9"
	create := func(name, role string) string {
		_, loc, _ := admin.do("POST", "/users", url.Values{"username": {name}, "role": {role}, "password": {pw}, "password2": {pw}})
		return loc
	}
	for _, u := range [][2]string{{"admin2", "admin"}, {"v1", "viewer"}} {
		if loc := create(u[0], u[1]); !strings.Contains(loc, "msg=") {
			t.Fatalf("create %s: %s", u[0], loc)
		}
	}
	if _, loc, _ := admin.do("POST", "/settings/four-eyes", url.Values{"four_eyes": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("turn on: %s", loc)
	}
	userID := func(name string) int64 {
		users, _ := e.store.ListUsers(ctx)
		for _, u := range users {
			if u.Username == name {
				return u.ID
			}
		}
		return 0
	}
	pending := func() int64 {
		var id int64
		e.pool.QueryRow(ctx, `SELECT id FROM approvals WHERE status='pending' ORDER BY id DESC LIMIT 1`).Scan(&id)
		return id
	}
	held := func(what, loc string) {
		t.Helper()
		if !strings.Contains(loc, "msg=Waiting") {
			t.Fatalf("%s not held: %s", what, loc)
		}
	}
	admin2 := newClient(t, e)
	admin2.login("admin2", pw)

	// A user who cannot approve is created at once; an approver waits.
	if loc := create("restorer", "restore"); !strings.Contains(loc, "msg=User") {
		t.Errorf("restore operator: %s", loc)
	}
	held("new administrator", create("sock", "admin"))
	if userID("sock") != 0 {
		t.Fatal("administrator created without approval")
	}
	createReq := pending()
	if loc := create("sock", "admin"); !strings.Contains(loc, "err=") {
		t.Errorf("same request twice: %s", loc)
	}

	// More rights, password and two-factor resets of approvers.
	v1 := userID("v1")
	_, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d", v1), url.Values{"role": {"admin"}})
	held("role change", loc)
	roleReq := pending()
	if u, _ := e.store.GetUser(ctx, v1); u.Role != "viewer" {
		t.Error("role changed without approval")
	}
	a2 := userID("admin2")
	_, loc, _ = admin.do("POST", fmt.Sprintf("/users/%d/password", a2), url.Values{"password": {"new-secret-77x"}, "password2": {"new-secret-77x"}})
	held("password reset", loc)
	var payload string
	e.pool.QueryRow(ctx, `SELECT payload::text FROM approvals WHERE id=$1`, pending()).Scan(&payload)
	if strings.Contains(payload, "new-secret-77x") || !strings.Contains(payload, "$2a$") {
		t.Errorf("password stored in the request: %s", payload)
	}
	if c := newClient(t, e); !c.login("admin2", pw) {
		t.Error("password changed without approval")
	}
	_, loc, _ = admin.do("POST", fmt.Sprintf("/users/%d/2fa-reset", a2), url.Values{})
	held("two-factor reset", loc)
	// A viewer's password is reset at once.
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d/password", v1), url.Values{"password": {"viewer-pass-88"}, "password2": {"viewer-pass-88"}}); !strings.Contains(loc, "msg=Password") {
		t.Errorf("viewer password: %s", loc)
	}

	// The last two administrators stay.
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d", a2), url.Values{"role": {"viewer"}}); !strings.Contains(loc, "at+least+two+enabled+administrators") {
		t.Errorf("second administrator lowered: %s", loc)
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d/delete", a2), url.Values{}); !strings.Contains(loc, "at+least+two+enabled+administrators") {
		t.Errorf("second administrator deleted: %s", loc)
	}

	// The second administrator approves the new account; it signs in with
	// the password given in the request.
	time.Sleep(10 * time.Millisecond)
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", createReq), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve creation: %s", loc)
	}
	sock := newClient(t, e)
	if !sock.login("sock", pw) {
		t.Fatal("approved account cannot sign in")
	}
	// It cannot decide what was requested before it existed...
	if _, loc, _ := sock.do("POST", fmt.Sprintf("/approvals/%d/approve", roleReq), url.Values{}); !strings.Contains(loc, "err=") {
		t.Error("an account created after the request decided it")
	}
	if _, _, body := sock.do("GET", "/approvals", nil); strings.Contains(body, fmt.Sprintf("/approvals/%d/approve", roleReq)) {
		t.Error("approve button offered to a newer account")
	}
	// ...but what is requested later.
	time.Sleep(10 * time.Millisecond)
	_, loc, _ = admin.do("POST", fmt.Sprintf("/users/%d/delete", userID("restorer")), url.Values{})
	held("user deletion", loc)
	if _, loc, _ := sock.do("POST", fmt.Sprintf("/approvals/%d/approve", pending()), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Errorf("later request: %s", loc)
	}

	// LDAP changes wait; the bind password is not readable in the request.
	ldap := url.Values{"enabled": {"on"}, "url": {"ldaps://dc1.example.local:636"}, "bind_dn": {"cn=svc"}, "bind_password": {"ldap-bind-secret"},
		"base_dn": {"dc=example,dc=local"}, "admin_group": {"cn=Domain Users"}}
	_, loc, _ = admin.do("POST", "/settings/ldap", ldap)
	held("LDAP change", loc)
	e.pool.QueryRow(ctx, `SELECT payload::text FROM approvals WHERE id=$1`, pending()).Scan(&payload)
	if strings.Contains(payload, "ldap-bind-secret") || strings.Contains(payload, "Domain Users") {
		t.Errorf("LDAP settings readable in the request: %s", payload)
	}
	ldapURL := func() string {
		var l struct {
			URL string `json:"url"`
		}
		e.store.GetSetting(ctx, "ldap", &l)
		return l.URL
	}
	if ldapURL() != "" {
		t.Error("LDAP saved without approval")
	}
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", pending()), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve LDAP: %s", loc)
	}
	if ldapURL() != "ldaps://dc1.example.local:636" {
		t.Errorf("approved LDAP change not saved: %q", ldapURL())
	}

	// Disabling a job waits.
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "store", Kind: "local", URL: t.TempDir()})
	jid, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobFiles, AgentID: agents[0].ID, TargetID: target, Name: "docs", Paths: []string{t.TempDir()}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, loc, _ = admin.do("POST", fmt.Sprintf("/jobs/%d/disable", jid), url.Values{})
	held("job disable", loc)
	if j, _ := e.store.GetJob(ctx, jid); !j.Enabled {
		t.Error("job disabled without approval")
	}
	admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", pending()), url.Values{})
	if j, _ := e.store.GetJob(ctx, jid); j.Enabled {
		t.Error("approved disable not carried out")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/enable", jid), url.Values{}); !strings.Contains(loc, "msg=Job+enabled") {
		t.Errorf("enabling: %s", loc)
	}

	// The settings page explains the rules.
	if _, _, body := admin.do("GET", "/settings/security", nil); !strings.Contains(body, "Only accounts that existed before a request can decide it.") {
		t.Error("settings page lacks the rules")
	}
}
