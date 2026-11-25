package server_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
)

// A VM backup that a restore test booted and checked successfully is marked
// "verified" on its page, where it is restored from; one whose boot test
// failed is not, also when another VM of the same test passed (0.61).
func TestBootVerifiedMark(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.61.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	backup := func(snap string) int64 {
		var id int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, status, repo_url, snapshot_id, started_at, finished_at)
			VALUES($1,'vm-backup','success','/repo',$2,now(),now()) RETURNING id`, a.ID, snap).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	good, bad := backup("aaaa1111"), backup("bbbb2222")
	if _, err := e.pool.Exec(ctx, `INSERT INTO runs(agent_id, kind, status, repo_url, snapshot_id, started_at, finished_at, details)
		VALUES($1,'verify','failed','/repo','aaaa1111',now(),now(),$2::jsonb)`, a.ID,
		`{"boot":[{"vmid":100,"snapshot":"aaaa1111","booted":true,"passed":true},{"vmid":100,"snapshot":"bbbb2222","booted":true,"checks":[{"check":"service nginx","ok":false}]}]}`); err != nil {
		t.Fatal(err)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", good), nil); !strings.Contains(body, "passed their checks in a restore test") {
		t.Error("the tested backup is not marked verified")
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", bad), nil); strings.Contains(body, "passed their checks in a restore test") {
		t.Error("a backup whose checks failed is marked verified")
	}
}
