package server_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
)

// B9: a code made for one machine works once; revoking removes every code,
// enrolled agents keep working.
func TestEnrollmentCodes(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	once, err := e.store.CreateSingleUseToken(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Enroll(ctx, e.ts.URL, once, e.fp, "test"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := agent.Enroll(ctx, e.ts.URL, once, e.fp, "test"); err == nil {
		t.Error("a single-use code worked twice")
	}
	multi, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := agent.Enroll(ctx, e.ts.URL, multi, e.fp, "test"); err != nil {
			t.Fatalf("multi-use code, use %d: %v", i+1, err)
		}
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", "/agents/tokens/revoke", url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("revoke: %s", loc)
	}
	if _, err := agent.Enroll(ctx, e.ts.URL, multi, e.fp, "test"); err == nil {
		t.Error("revoked code still works")
	}
	if agents, _ := e.store.ListAgents(ctx); len(agents) != 3 {
		t.Errorf("%d agents after revoking, want 3", len(agents))
	}
}
