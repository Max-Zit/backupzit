package server_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// A Hyper-V VM job can list VMs whose crash-consistent checkpoint is
// expected (issue #41: old guests without VSS gave a warning on every run).
// The list is kept, shown in the edit form, sent to the agent with the run,
// and a broken pattern is refused.
func TestCrashConsistentOKList(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.53.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	inv := `{"platform":"hyperv","node":"HV1","guests":[{"vmid":1000000001,"id":"guid-old","name":"old-centos","type":"vm","node":"HV1","status":"running"}],"storage":["D:/VMs"]}`
	if err := e.store.TouchAgent(ctx, agents[0].ID, api.PollRequest{Hostname: "HV1", OS: "Windows Server 2025", Hypervisor: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	_, loc, _ := c.do("POST", "/jobs", url.Values{"name": {"hyper-v"}, "agent_id": {fmt.Sprint(agents[0].ID)}, "target_id": {fmt.Sprint(target)},
		"kind": {"vm"}, "vm_mode": {"all"}, "crash_consistent_ok": {"old-centos\n  legacy-*  \n"}, "sched_kind": {"manual"}})
	if !strings.Contains(loc, "/jobs/") {
		msg, _ := url.QueryUnescape(loc)
		t.Fatalf("create: %s", msg)
	}
	jobs, _ := e.store.ListJobs(ctx)
	j := jobs[0]
	if strings.Join(j.Options.CrashConsistentOK, "|") != "old-centos|legacy-*" {
		t.Fatalf("list %q", j.Options.CrashConsistentOK)
	}
	_, _, page := c.do("GET", fmt.Sprintf("/jobs/%d/edit", j.ID), nil)
	if !strings.Contains(page, `name="crash_consistent_ok"`) || !strings.Contains(page, "legacy-*") {
		t.Error("edit form does not show the list")
	}
	rid, _ := e.store.QueueBackup(ctx, j.ID, "manual")
	if _, err := e.store.ClaimRun(ctx, agents[0].ID); err != nil {
		t.Fatal(err)
	}
	ar, err := e.srv.APIRun(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ar.VMCrashConsistentOK, "|") != "old-centos|legacy-*" {
		t.Errorf("run for the agent: %q", ar.VMCrashConsistentOK)
	}
	// A broken pattern is refused with a reason.
	_, loc, _ = c.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), url.Values{"name": {"hyper-v"}, "vm_mode": {"all"},
		"crash_consistent_ok": {"[broken"}, "sched_kind": {"manual"}})
	if msg, _ := url.QueryUnescape(loc); !strings.Contains(msg, "not a VM name or pattern") {
		t.Errorf("broken pattern: %s", msg)
	}
}
