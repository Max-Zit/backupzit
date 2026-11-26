package server_test

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// A job for a Hyper-V failover cluster gives every node of the cluster a
// member job; all members back up into one repository of the cluster, so a
// VM's history continues when it moves to another node.
func TestHyperVClusterJob(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	enroll := func(host, inv string) server.Agent {
		cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
		if err != nil {
			t.Fatal(err)
		}
		agents, _ := e.store.ListAgents(ctx)
		for _, a := range agents {
			if a.UUID == cfg.AgentUUID {
				if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Hostname: host, OS: "Windows Server 2022", Hypervisor: json.RawMessage(inv)}, ""); err != nil {
					t.Fatal(err)
				}
				a, _ = e.store.GetAgent(ctx, a.ID)
				return a
			}
		}
		t.Fatal("agent missing")
		return server.Agent{}
	}
	vm := func(vmid int, name, node string, clustered bool) string {
		b, _ := json.Marshal(map[string]any{"vmid": vmid, "id": "guid-" + name, "name": name, "type": "vm", "node": node, "status": "running", "clustered": clustered})
		return string(b)
	}
	n1 := enroll("HV1", `{"platform":"hyperv","node":"HV1","cluster_name":"HVC","guests":[`+vm(1000000001, "web", "HV1", true)+`,`+vm(1000000003, "local-only", "HV1", false)+`],"storage":["C:\\ClusterStorage\\Volume1"]}`)
	n2 := enroll("HV2", `{"platform":"hyperv","node":"HV2","cluster_name":"HVC","guests":[`+vm(1000000002, "db", "HV2", true)+`],"storage":["C:\\ClusterStorage\\Volume1"]}`)
	other := enroll("HV9", `{"platform":"hyperv","node":"HV9","guests":[`+vm(1000000009, "lab", "HV9", false)+`],"storage":["D:\\VMs"]}`)
	if n1.HVCluster() != "HVC" || n2.HVCluster() != "HVC" || other.HVCluster() != "" {
		t.Fatalf("cluster names %q %q %q", n1.HVCluster(), n2.HVCluster(), other.HVCluster())
	}

	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	_, _, page := c.do("GET", "/jobs", nil)
	if !strings.Contains(page, `value="hvcluster:HVC"`) || !strings.Contains(page, `data-hvnode="HVC"`) {
		t.Fatal("the new job form does not offer the cluster")
	}
	// The cluster's VM list holds the clustered VMs of both nodes only.
	if !strings.Contains(page, `"hvcluster:HVC":[`) || !strings.Contains(page, `"name":"db"`) {
		t.Fatal("cluster VMs missing from the form data")
	}
	_, loc, _ := c.do("POST", "/jobs", url.Values{"name": {"cluster VMs"}, "agent_id": {"hvcluster:HVC"}, "target_id": {itoa(target)},
		"kind": {"vm"}, "vm_mode": {"selected"}, "vms": {"1000000001", "1000000002"}, "sched_kind": {"manual"}, "vm_block_size_kib": {"256"}})
	if !strings.Contains(loc, "/job-groups/") || !strings.Contains(loc, "msg=") {
		msg, _ := url.QueryUnescape(loc)
		t.Fatalf("create: %s", msg)
	}
	groups, _ := e.store.ListJobGroups(ctx)
	if len(groups) != 1 || groups[0].Scope != server.GroupHVCluster || groups[0].Options.HVCluster != "HVC" || groups[0].Kind != server.JobVM {
		t.Fatalf("group %+v", groups)
	}
	jobs, _ := e.store.ListJobs(ctx)
	var members []server.Job
	for _, j := range jobs {
		if j.GroupID != nil {
			members = append(members, j)
		}
	}
	if len(members) != 2 {
		t.Fatalf("%d member jobs, want one per node", len(members))
	}
	repos := map[string]bool{}
	for _, m := range members {
		if m.AgentID == other.ID {
			t.Fatal("an agent outside the cluster got a member job")
		}
		if m.Options.HVCluster != "HVC" || m.Options.VMBlockSizeKiB != 256 || strings.Join(m.Paths, ",") != "1000000001,1000000002" {
			t.Fatalf("member %+v", m)
		}
		id, err := e.store.QueueBackup(ctx, m.ID, "manual")
		if err != nil {
			t.Fatal(err)
		}
		run, _ := e.store.GetRun(ctx, id)
		repos[run.RepoURL] = true
	}
	if len(repos) != 1 {
		t.Fatalf("members use %d repositories, want the cluster's one: %v", len(repos), repos)
	}
	for r := range repos {
		if !strings.HasSuffix(r, "hvcluster-hvc") {
			t.Fatalf("repository %s", r)
		}
	}
	gid := groups[0].ID
	report := func(a server.Agent, cluster, guests string) {
		t.Helper()
		inv := `{"platform":"hyperv","node":"` + a.Hostname + `","cluster_name":"` + cluster + `","guests":[` + guests + `]}`
		if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Hypervisor: json.RawMessage(inv)}, ""); err != nil {
			t.Fatal(err)
		}
	}
	web := vm(1000000001, "web", "HV1", true)
	db1 := vm(1000000002, "db", "HV1", true)
	db2 := vm(1000000002, "db", "HV2", true)
	syncTime := func(at time.Time) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, `UPDATE agents SET last_seen_at=$1, hypervisor_at=$1 WHERE id IN ($2,$3)`, at, n1.ID, n2.ID); err != nil {
			t.Fatal(err)
		}
	}
	warning := "Selected VM 1000000002 is not reported as clustered by any participating node of Hyper-V cluster HVC."
	checkWarning := func(want bool) {
		t.Helper()
		syncTime(time.Now())
		for _, path := range []string{"/", "/job-groups/" + itoa(gid)} {
			_, _, page := c.do("GET", path, nil)
			if strings.Contains(page, warning) != want {
				t.Errorf("%s: missing VM warning present = %v, want %v", path, strings.Contains(page, warning), want)
			}
			if strings.Contains(page, "Cannot verify selected VMs 1000000002") {
				t.Errorf("%s: fresh inventory produced unknown warning", path)
			}
		}
	}
	checkWarning(false)
	report(n2, "HVC", "")
	checkWarning(true)
	report(n1, "HVC", web+","+db1)
	checkWarning(false)
	report(n1, "HVC", web)
	report(n2, "HVC", "")
	syncTime(time.Now())
	if _, err := e.pool.Exec(ctx, `UPDATE agents SET hypervisor_at=$1 WHERE id=$2`, time.Now().Add(-time.Hour), n2.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/job-groups/" + itoa(gid)} {
		_, _, page := c.do("GET", path, nil)
		if !strings.Contains(page, "Cannot verify selected VMs 1000000002") || strings.Contains(page, warning) {
			t.Errorf("%s: stale inventory must be unknown, not missing", path)
		}
	}
	report(n2, "OTHER", db2)
	checkWarning(true)
	report(n2, "HVC", db2)
	checkWarning(false)
	report(n2, "HVC", "")
	checkWarning(true)
	// Editing keeps the cluster; a cluster job cannot back up files.
	_, loc, _ = c.do("POST", "/job-groups/"+itoa(gid)+"/edit", url.Values{"name": {"cluster VMs"}, "scope": {"hvcluster"},
		"vm_mode": {"all"}, "sched_kind": {"manual"}, "vm_block_size_kib": {"512"}})
	if g, _ := e.store.GetJobGroup(ctx, gid); !strings.Contains(loc, "msg=") || g.Options.HVCluster != "HVC" || g.Options.VMBlockSizeKiB != 512 || strings.Join(g.Paths, ",") != "*" {
		t.Fatalf("edit: %s %+v", loc, g)
	}
	checkWarning(false)
	report(n2, "HVC", db2)
	updated, err := e.store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range updated {
		if m.GroupID != nil && *m.GroupID == gid && m.Options.VMBlockSizeKiB != 512 {
			t.Fatalf("cluster member retained old block size: %+v", m)
		}
	}
	if _, err := e.store.CreateJobGroup(ctx, server.JobGroup{Kind: server.JobFiles, Name: "x", Scope: server.GroupHVCluster, TargetID: target, Paths: []string{"C:\\"},
		Options: server.JobOptions{HVCluster: "HVC"}}); err == nil {
		t.Error("files job for a cluster accepted")
	}
	if _, err := e.store.CreateJobGroup(ctx, server.JobGroup{Kind: server.JobVM, Name: "x", Scope: server.GroupAll, TargetID: target, Paths: []string{"*"}}); err == nil {
		t.Error("VM job for all agents accepted")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
