package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/server"
)

// TestVMwareParts: a vCenter connection with a further proxy splits a job's
// backup by ESXi host (V-1): one run per proxy in one batch, each with its
// part for retention; the job shows the start as a whole, and a job
// chained after it starts once, after the last part.
func TestVMwareParts(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	if !admin.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}
	targetID, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	type proxy struct {
		cfg *agent.Config
		id  int64
	}
	enroll := func(name string) proxy {
		token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
		cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, name)
		if err != nil {
			t.Fatal(err)
		}
		agents, _ := e.store.ListAgents(ctx)
		for _, a := range agents {
			if a.UUID == cfg.AgentUUID {
				return proxy{cfg, a.ID}
			}
		}
		t.Fatal("enrolled agent not found")
		return proxy{}
	}
	poll := func(p proxy, version, machine string) *api.Run {
		t.Helper()
		b, _ := json.Marshal(api.PollRequest{Hostname: fmt.Sprintf("proxy%d", p.id), OS: "Linux", Arch: "amd64", Version: version, MachineUUID: machine})
		req, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+p.cfg.AgentUUID+":"+p.cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var pr api.PollResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		return pr.Run
	}
	const uuidA, uuidB = "aaaaaaaa-1111-2222-3333-000000000001", "bbbbbbbb-1111-2222-3333-000000000002"
	pa, pb, old := enroll("a"), enroll("b"), enroll("old")
	poll(pa, "1.0.0", uuidA)
	poll(pb, "1.0.0", uuidB)
	poll(old, "0.54.2", "")

	inv, _ := json.Marshal(pve.Inventory{Platform: "vmware", Node: "vc1", Version: "vCenter 8.0.3", Hosts: []string{"esx1", "esx2"}, Guests: []pve.Guest{
		{VMID: 1, Name: "proxy-a", Node: "esx1", BIOSUUID: uuidA}, {VMID: 2, Name: "proxy-b", Node: "esx2", BIOSUUID: uuidB},
		{VMID: 10, Name: "web", Node: "esx1"}, {VMID: 11, Name: "db", Node: "esx2"}, {VMID: 12, Name: "mail", Node: "esx2"},
	}})
	hostID, err := e.store.CreateVMwareHost(ctx, server.VMwareHost{Name: "vc1", Address: "vc1.example", Username: "administrator@vsphere.local",
		Password: "x", ProxyAgentID: &pa.id, RepoDir: "esxi-vc1_0001", Inventory: inv, Version: "8.0.3", APIWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	// The Agents page offers the Linux agents with their ESXi host; an old
	// agent cannot be a further proxy.
	if _, _, body := admin.do("GET", "/agents", nil); !strings.Contains(body, fmt.Sprintf(`action="/vmware/%d/proxies"`, hostID)) || !strings.Contains(body, "on esx2") {
		t.Error("agents page lacks the further proxies of the vCenter connection")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/vmware/%d/proxies", hostID), url.Values{"agent_id": {fmt.Sprint(old.id)}}); !strings.Contains(loc, "err=") {
		t.Errorf("old agent accepted as a further proxy: %s", loc)
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/vmware/%d/proxies", hostID), url.Values{"agent_id": {fmt.Sprint(pb.id), fmt.Sprint(pa.id)}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("further proxy: %s", loc)
	}
	if h, _ := e.store.GetVMwareHost(ctx, hostID); !slices.Equal(h.ExtraProxies, []int64{pb.id}) {
		t.Fatalf("further proxies: %v (the connection's own proxy is not one)", h.ExtraProxies)
	}

	_, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"vc vms"}, "agent_id": {fmt.Sprintf("vmware:%d", hostID)}, "target_id": {fmt.Sprint(targetID)},
		"kind": {"vm"}, "vm_mode": {"all"}, "keep_last": {"3"}, "sched_kind": {"manual"}})
	m := regexp.MustCompile(`^/jobs/(\d+)`).FindStringSubmatch(loc)
	if m == nil {
		t.Fatalf("create job: %s", loc)
	}
	var jobID int64
	fmt.Sscan(m[1], &jobID)
	// A job chained after it, on the proxy too.
	_, loc, _ = admin.do("POST", "/jobs", url.Values{"name": {"after"}, "agent_id": {fmt.Sprintf("vmware:%d", hostID)}, "target_id": {fmt.Sprint(targetID)},
		"kind": {"vm"}, "vm_mode": {"selected"}, "vms": {"10"}, "keep_last": {"3"}, "sched_kind": {"chain"}, "sched_after_job": {fmt.Sprint(jobID)}})
	m = regexp.MustCompile(`^/jobs/(\d+)`).FindStringSubmatch(loc)
	if m == nil {
		t.Fatalf("create chained job: %s", loc)
	}
	var afterID int64
	fmt.Sscan(m[1], &afterID)

	first, err := e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); !errors.Is(err, server.ErrRunActive) {
		t.Errorf("second start while the parts run: %v", err)
	}
	type row struct {
		id, agent int64
		part      string
		batch     int64
		paths     []string
		excludes  []string
	}
	rows, _ := e.pool.Query(ctx, `SELECT id, agent_id, part, coalesce(batch,0), paths, excludes FROM runs WHERE job_id=$1 ORDER BY id`, jobID)
	var got []row
	for rows.Next() {
		var r row
		rows.Scan(&r.id, &r.agent, &r.part, &r.batch, &r.paths, &r.excludes)
		got = append(got, r)
	}
	rows.Close()
	if len(got) != 2 || got[0].id != first || got[0].agent != pa.id || got[1].agent != pb.id || got[0].batch != first || got[1].batch != first ||
		got[0].part != fmt.Sprintf("proxy:%d", pa.id) || got[1].part != fmt.Sprintf("proxy:%d", pb.id) ||
		!slices.Equal(got[0].paths, []string{"*"}) || !slices.Equal(got[0].excludes, []string{"11", "12", "2"}) || !slices.Equal(got[1].paths, []string{"2", "11", "12"}) {
		t.Fatalf("parts: %+v", got)
	}

	ra := poll(pa, "1.0.0", uuidA)
	if ra == nil || ra.ID != first || ra.Part != got[0].part || !ra.PartPrimary || !slices.Equal(ra.PartOthers, []string{got[1].part}) || ra.VMware == nil {
		t.Fatalf("part of the connection's proxy: %+v", ra)
	}
	rb := poll(pb, "1.0.0", uuidB)
	if rb == nil || rb.ID != got[1].id || rb.Part != got[1].part || rb.PartPrimary || rb.PartOthers != nil || !slices.Equal(rb.VMs, []string{"2", "11", "12"}) {
		t.Fatalf("part of the further proxy: %+v", rb)
	}
	status := func() (string, bool) {
		j, _ := e.store.GetJob(ctx, jobID)
		s := ""
		if j.LastStatus != nil {
			s = *j.LastStatus
		}
		return s, j.LastFinished != nil
	}
	snap := func(c byte) string { return strings.Repeat(string(c), 64) }
	e.store.FinishRun(ctx, pa.id, first, api.RunResult{Status: api.StatusSuccess, SnapshotID: snap('a'), Stats: json.RawMessage(`{}`)})
	if s, fin := status(); s != "running" || fin {
		t.Errorf("one part done: %s finished=%v, want running", s, fin)
	}
	if ids, _ := e.store.ChainAfterRun(ctx, first); len(ids) != 0 {
		t.Errorf("chain started before the last part: %v", ids)
	}
	e.store.FinishRun(ctx, pb.id, got[1].id, api.RunResult{Status: api.StatusFailed, Message: "boom"})
	if s, fin := status(); s != "failed" || !fin {
		t.Errorf("a part failed: %s finished=%v, want failed", s, fin)
	}
	// The chain after a failed part does not start (only on success); it is
	// decided once.
	if ids, _ := e.store.ChainAfterRun(ctx, got[1].id); len(ids) != 0 {
		t.Errorf("chain after a failed part: %v", ids)
	}

	// Next start: both succeed; the chain follows once.
	first, err = e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	ra, rb = poll(pa, "1.0.0", uuidA), poll(pb, "1.0.0", uuidB)
	if ra == nil || rb == nil || ra.ID != first {
		t.Fatalf("parts not handed out: %+v %+v", ra, rb)
	}
	e.store.FinishRun(ctx, pa.id, ra.ID, api.RunResult{Status: api.StatusSuccess, SnapshotID: snap('b'), Stats: json.RawMessage(`{}`)})
	e.store.FinishRun(ctx, pb.id, rb.ID, api.RunResult{Status: api.StatusWarning, SnapshotID: snap('c'), Stats: json.RawMessage(`{}`)})
	if s, _ := status(); s != "warning" {
		t.Errorf("both parts done: %s, want warning", s)
	}
	ids, err := e.store.ChainAfterRun(ctx, rb.ID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("chain after the last part: %v %v", ids, err)
	}
	if again, _ := e.store.ChainAfterRun(ctx, ra.ID); len(again) != 0 {
		t.Errorf("chain started twice: %v", again)
	}
	e.pool.Exec(ctx, `UPDATE runs SET status='cancelled' WHERE job_id=$1 AND status IN ('queued','running')`, afterID)

	// A copy of the job runs per part too, each keeping its part.
	offsite, _ := e.store.CreateTarget(ctx, server.Target{Name: "offsite", Kind: "local", URL: t.TempDir()})
	_, loc, _ = admin.do("POST", "/jobs", url.Values{"name": {"vc copy"}, "kind": {"copy"}, "source_job": {fmt.Sprint(jobID)},
		"target_id": {fmt.Sprint(offsite)}, "sched_kind": {"manual"}, "keep_last": {"2"}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("create copy job: %s", loc)
	}
	var copyJob int64
	e.pool.QueryRow(ctx, `SELECT id FROM jobs WHERE name='vc copy'`).Scan(&copyJob)
	cfirst, err := e.store.QueueBackup(ctx, copyJob, "manual")
	if err != nil {
		t.Fatal(err)
	}
	var cparts []string
	e.pool.QueryRow(ctx, `SELECT array_agg(part ORDER BY id) FROM runs WHERE batch=$1 AND kind='copy' AND agent_id=$2`, cfirst, pa.id).Scan(&cparts)
	if !slices.Equal(cparts, []string{got[0].part, got[1].part}) {
		t.Fatalf("copy parts: %v", cparts)
	}
	c1, c2 := poll(pa, "1.0.0", uuidA), poll(pa, "1.0.0", uuidA)
	if c1 != nil && c2 != nil && c2.ID < c1.ID {
		c1, c2 = c2, c1
	}
	if c1 == nil || c2 == nil || c1.Kind != api.KindCopy || c1.Part != got[0].part || !c1.PartPrimary || !slices.Equal(c1.PartOthers, []string{got[1].part}) ||
		c2.Part != got[1].part || c2.PartPrimary || c1.SourceTag != fmt.Sprintf("job:%d", jobID) {
		t.Fatalf("copy runs: %+v / %+v", c1, c2)
	}
	e.store.FinishRun(ctx, pa.id, c1.ID, api.RunResult{Status: api.StatusSuccess, SnapshotID: snap('d'), Stats: json.RawMessage(`{}`)})
	e.store.FinishRun(ctx, pa.id, c2.ID, api.RunResult{Status: api.StatusSuccess, SnapshotID: snap('e'), Stats: json.RawMessage(`{}`)})

	// An old agent as the connection's proxy refuses its part (its
	// retention would remove the other proxy's backups).
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/vmware/%d/proxy", hostID), url.Values{"proxy_agent_id": {fmt.Sprint(old.id)}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("change proxy: %s", loc)
	}
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != nil {
		t.Fatal(err)
	}
	if r := poll(old, "0.54.2", ""); r != nil {
		t.Errorf("old proxy got a part: %+v", r)
	}
	var msg string
	e.pool.QueryRow(ctx, `SELECT message FROM runs WHERE job_id=$1 AND agent_id=$2 ORDER BY id DESC LIMIT 1`, jobID, old.id).Scan(&msg)
	if !strings.Contains(msg, "several proxies") {
		t.Errorf("old proxy's part: %q", msg)
	}
}
