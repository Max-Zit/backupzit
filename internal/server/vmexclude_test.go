package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// A VM job can back up every VM except the ticked ones (issue #28) and
// leave out Hyper-V disks by pattern (issue #29); the edit form shows both,
// and an agent too old to exclude disks is refused instead of backing up
// every disk.
func TestVMExceptAndExcludeDisks(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	inv := `{"platform":"hyperv","node":"HV1","guests":[` +
		`{"vmid":1000000001,"id":"guid-console","name":"console","type":"vm","node":"HV1","status":"running"},` +
		`{"vmid":1000000002,"id":"guid-sql","name":"sql","type":"vm","node":"HV1","status":"running"}],"storage":["D:/VMs"]}`
	if err := e.store.TouchAgent(ctx, agents[0].ID, api.PollRequest{Hostname: "HV1", OS: "Windows Server 2025", Hypervisor: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}

	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	_, loc, _ := c.do("POST", "/jobs", url.Values{"name": {"all but console"}, "agent_id": {fmt.Sprint(agents[0].ID)}, "target_id": {fmt.Sprint(target)},
		"kind": {"vm"}, "vm_mode": {"except"}, "vms": {"1000000001"}, "exclude_disks": {"*-data.vhdx\nD:\\VMs\\sql\\logs.vhdx"}, "sched_kind": {"manual"}})
	if !strings.Contains(loc, "/jobs/") {
		msg, _ := url.QueryUnescape(loc)
		t.Fatalf("create: %s", msg)
	}
	jobs, _ := e.store.ListJobs(ctx)
	if len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
	j := jobs[0]
	if strings.Join(j.Paths, ",") != "*" || strings.Join(j.Excludes, ",") != "1000000001" {
		t.Fatalf("selection: paths %v excludes %v", j.Paths, j.Excludes)
	}
	if strings.Join(j.Options.ExcludeDisks, "|") != `*-data.vhdx|D:\VMs\sql\logs.vhdx` {
		t.Fatalf("exclude disks %q", j.Options.ExcludeDisks)
	}

	_, _, page := c.do("GET", fmt.Sprintf("/jobs/%d", j.ID), nil)
	if !strings.Contains(page, "on the host except 1") || !strings.Contains(page, "*-data.vhdx") {
		t.Error("job page does not show the exception or the excluded disks")
	}
	_, _, page = c.do("GET", fmt.Sprintf("/jobs/%d/edit", j.ID), nil)
	if !strings.Contains(page, `value="except" checked`) || !strings.Contains(page, `value="1000000001" checked`) || !strings.Contains(page, `*-data.vhdx`) {
		t.Error("edit form does not show the exception and the excluded disks")
	}

	// Saving the edit form keeps everything.
	c.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), url.Values{"name": {"all but console"}, "vm_mode": {"except"}, "vms": {"1000000001"},
		"exclude_disks": {"*-data.vhdx"}, "sched_kind": {"manual"}})
	j2, _ := e.store.GetJob(ctx, j.ID)
	if strings.Join(j2.Excludes, ",") != "1000000001" || strings.Join(j2.Options.ExcludeDisks, ",") != "*-data.vhdx" {
		t.Fatalf("after edit: excludes %v disks %v", j2.Excludes, j2.Options.ExcludeDisks)
	}
	// "All VMs" clears the exception.
	c.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), url.Values{"name": {"all"}, "vm_mode": {"all"}, "vms": {"1000000001"},
		"exclude_disks": {"*-data.vhdx"}, "sched_kind": {"manual"}})
	if j3, _ := e.store.GetJob(ctx, j.ID); len(j3.Excludes) != 0 || strings.Join(j3.Paths, ",") != "*" {
		t.Fatalf("all VMs: paths %v excludes %v", j3.Paths, j3.Excludes)
	}

	// An agent older than 0.49.0 cannot exclude disks: refused with a reason.
	old := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "0.48.2")
	rid, _ := e.store.QueueBackup(ctx, j.ID, "manual")
	runAgent(t, old)
	if r, _ := e.store.GetRun(ctx, rid); r.Status != api.StatusFailed || !strings.Contains(r.Message, "cannot exclude disks yet") {
		t.Errorf("old agent: %s %q", r.Status, r.Message)
	}
}
