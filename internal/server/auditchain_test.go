package server_test

import (
	"bufio"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestAuditChain: entries form a chain that detects changed, deleted and
// truncated entries, and every entry goes to the syslog server.
func TestAuditChain(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	// A syslog server (TCP) receiving the copy.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 100)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sc := bufio.NewScanner(c)
		sc.Split(bufio.ScanWords)
		var line strings.Builder
		for sc.Scan() {
			line.WriteString(sc.Text() + " ")
			if strings.Contains(sc.Text(), "hash=") {
				got <- line.String()
				line.Reset()
			}
		}
	}()
	e.srv.StartAuditChain(ctx, t.TempDir())
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", "/settings/audit-syslog", url.Values{"address": {ln.Addr().String()}, "protocol": {"tcp"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("syslog settings: %s", loc)
	}
	for i := 0; i < 3; i++ {
		newClient(t, e).login("admin", "admin-pass-123") // each sign-in is an entry
	}
	if res := e.store.VerifyAudit(ctx); !res.OK || res.Entries < 4 {
		t.Fatalf("fresh chain: %+v", res)
	}
	if _, loc, _ := admin.do("POST", "/audit/verify", nil); !strings.Contains(loc, "msg=") {
		t.Errorf("verify button: %s", loc)
	}
	_, _, body := admin.do("GET", "/audit", nil)
	if !strings.Contains(body, "Integrity verified") {
		t.Error("audit page lacks the integrity result")
	}
	// The syslog copy carries the entries with their hashes.
	deadline := time.After(5 * time.Second)
	seen := 0
	for seen < 3 {
		select {
		case l := <-got:
			if strings.Contains(l, "action=") {
				seen++
			}
		case <-deadline:
			t.Fatalf("syslog received %d entries", seen)
		}
	}

	// Changing an entry in the database is detected.
	var mid int64
	e.pool.QueryRow(ctx, `SELECT id FROM audit_log WHERE hash <> '' ORDER BY id OFFSET 1 LIMIT 1`).Scan(&mid)
	e.pool.Exec(ctx, `UPDATE audit_log SET detail = 'nothing happened' WHERE id = $1`, mid)
	if res := e.store.VerifyAudit(ctx); res.OK || res.BadID != mid || !strings.Contains(res.Problem, "changed") {
		t.Errorf("changed entry: %+v", res)
	}
	if _, loc, _ := admin.do("POST", "/audit/verify", nil); !strings.Contains(loc, "err=") {
		t.Errorf("verify button after a change: %s", loc)
	}
	// Deleting an entry in the middle is detected.
	e.pool.Exec(ctx, `DELETE FROM audit_log WHERE id = $1`, mid)
	if res := e.store.VerifyAudit(ctx); res.OK || !strings.Contains(res.Problem, "deleted") {
		t.Errorf("deleted entry: %+v", res)
	}
}

// TestAuditChainTruncated: deleting the newest entries is detected by the
// head kept outside the database.
func TestAuditChainTruncated(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	e.srv.StartAuditChain(ctx, t.TempDir())
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	newClient(t, e).login("admin", "admin-pass-123")
	newClient(t, e).login("admin", "admin-pass-123")
	if res := e.store.VerifyAudit(ctx); !res.OK {
		t.Fatalf("fresh chain: %+v", res)
	}
	e.pool.Exec(ctx, `DELETE FROM audit_log WHERE id = (SELECT max(id) FROM audit_log)`)
	if res := e.store.VerifyAudit(ctx); res.OK || !strings.Contains(res.Problem, "newest") {
		t.Errorf("truncated chain: %+v", res)
	}
	// A new entry after the deletion does not hide it.
	newClient(t, e).login("admin", "admin-pass-123")
	if res := e.store.VerifyAudit(ctx); res.OK || !strings.Contains(res.Problem, "deleted") {
		t.Errorf("truncated chain after a new entry: %+v", res)
	}
}
