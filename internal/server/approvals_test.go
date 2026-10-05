package server_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// With four-eyes approval on, deleting a user waits for a second
// administrator; the requester cannot approve, a viewer cannot decide, and
// turning the option off needs approval as well.
func TestFourEyesApproval(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	fourEyes := func(c *client, on bool) string {
		f := url.Values{}
		if on {
			f.Set("four_eyes", "on")
		}
		_, loc, _ := c.do("POST", "/settings/four-eyes", f)
		return loc
	}
	if loc := fourEyes(admin, true); !strings.Contains(loc, "err=") {
		t.Fatalf("turned on with one administrator: %s", loc)
	}
	for _, u := range []struct{ name, role string }{{"admin2", "admin"}, {"victim", "viewer"}, {"viewer1", "viewer"}} {
		if _, loc, _ := admin.do("POST", "/users", url.Values{"username": {u.name}, "role": {u.role},
			"password": {"correct-horse-9"}, "password2": {"correct-horse-9"}}); !strings.Contains(loc, "msg=") {
			t.Fatalf("create %s: %s", u.name, loc)
		}
	}
	if loc := fourEyes(admin, true); !strings.Contains(loc, "msg=") {
		t.Fatalf("turn on: %s", loc)
	}
	userID := func(name string) int64 {
		users, err := e.store.ListUsers(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range users {
			if u.Username == name {
				return u.ID
			}
		}
		return 0
	}
	victim := userID("victim")
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d/delete", victim), url.Values{}); !strings.Contains(loc, "msg=Waiting") {
		t.Fatalf("delete was not held: %s", loc)
	}
	if userID("victim") == 0 {
		t.Fatal("user deleted without approval")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d/delete", victim), url.Values{}); !strings.Contains(loc, "err=") {
		t.Error("second identical request accepted")
	}
	pendingID := func() int64 {
		var id int64
		e.pool.QueryRow(e.ctx, `SELECT id FROM approvals WHERE status='pending' ORDER BY id DESC LIMIT 1`).Scan(&id)
		return id
	}
	id := pendingID()
	_, _, body := admin.do("GET", "/", nil)
	if !strings.Contains(body, `href="/approvals"`) || !strings.Contains(body, "1 change waits for a second user") {
		t.Error("pending approval not shown on the dashboard")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "err=") {
		t.Error("requester approved their own request")
	}
	viewer := newClient(t, e)
	viewer.login("viewer1", "correct-horse-9")
	if _, loc, _ := viewer.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "err=") {
		t.Error("viewer approved a user deletion")
	}
	admin2 := newClient(t, e)
	admin2.login("admin2", "correct-horse-9")
	if _, _, body := admin2.do("GET", "/approvals", nil); !strings.Contains(body, "victim") || !strings.Contains(body, fmt.Sprintf("/approvals/%d/approve", id)) {
		t.Error("request not offered to the second administrator")
	}
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve: %s", loc)
	}
	if userID("victim") != 0 {
		t.Error("user not deleted after approval")
	}
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "err=") {
		t.Error("request carried out twice")
	}

	// Turning the option off: rejected once, approved the second time.
	if loc := fourEyes(admin, false); !strings.Contains(loc, "msg=Waiting") {
		t.Fatalf("turning off was not held: %s", loc)
	}
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/reject", pendingID()), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("reject: %s", loc)
	}
	if _, _, body := admin.do("GET", "/settings/security", nil); !strings.Contains(body, `name="four_eyes" checked`) {
		t.Error("rejected request changed the setting")
	}
	fourEyes(admin, false)
	admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", pendingID()), url.Values{})
	if _, _, body := admin.do("GET", "/settings/security", nil); strings.Contains(body, `name="four_eyes" checked`) {
		t.Error("approved request did not turn the option off")
	}
	// Off again: deletions take effect at once.
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d/delete", userID("viewer1")), url.Values{}); !strings.Contains(loc, "msg=User") {
		t.Errorf("delete with approval off: %s", loc)
	}
	_, _, body = admin.do("GET", "/audit", nil)
	for _, a := range []string{"approval.request", "approval.approve", "approval.reject"} {
		if !strings.Contains(body, a) {
			t.Errorf("audit log lacks %s", a)
		}
	}
}
