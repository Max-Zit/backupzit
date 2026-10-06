package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// TestImageAllDisks: an image job of all disks is created from the form,
// its run shows every disk with browse links per disk, and a restore
// writes the chosen disk of the backup; old agents get a clear error.
func TestImageAllDisks(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	inv := `[{"number":0,"model":"System SSD","size":68719476736,"sector_size":512,"style":"gpt","system":true,
	  "partitions":[{"number":3,"offset":227540992,"length":67554508800,"mount_points":["C:\\"],"file_system":"NTFS"}]},
	  {"number":1,"model":"Data HDD","size":1099511627776,"sector_size":512,"style":"gpt",
	  "partitions":[{"number":2,"offset":1048576,"length":1099510579200,"mount_points":["D:\\"],"file_system":"NTFS"}]},
	  {"number":2,"model":"USB stick","size":32000000000,"sector_size":512,"style":"mbr","bus":"usb",
	  "partitions":[{"number":1,"offset":1048576,"length":31000000000,"mount_points":["E:\\"],"file_system":"exFAT"}]}]`
	if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Disks: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}

	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, loc, _ := admin.do("POST", "/jobs", url.Values{"kind": {"image"}, "name": {"Whole PC"}, "agent_id": {fmt.Sprint(a.ID)},
		"target_id": {fmt.Sprint(targetID)}, "image_disk": {"-1"}, "image_parts": {"3"}, "sched_kind": {"manual"}, "keep_last": {"3"}})
	if !strings.Contains(loc, "/jobs/") || strings.Contains(loc, "err=") {
		t.Fatalf("create job: %s", loc)
	}
	jobs, _ := e.store.ListJobs(ctx)
	j := jobs[0]
	if j.ImageDisk == nil || *j.ImageDisk != -1 || len(j.ImagePartitions) != 0 || server.DescribeImageSelection(j.ImageDisk, j.ImagePartitions) != "All disks" {
		t.Fatalf("job: disk %v parts %v", j.ImageDisk, j.ImagePartitions)
	}

	poll := func(version string) *api.Run {
		b, _ := json.Marshal(api.PollRequest{Hostname: "pc", Version: version})
		req, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var pr api.PollResponse
		json.Unmarshal(out, &pr)
		return pr.Run
	}
	// An agent too old for "all disks" is told to update.
	old, _ := e.store.QueueBackup(ctx, j.ID, "manual")
	if run := poll("0.33.5"); run != nil {
		t.Fatalf("old agent got the run: %+v", run)
	}
	if r, _ := e.store.GetRun(ctx, old); r.Status != "failed" || !strings.Contains(r.Message, "update it to 0.33.7") {
		t.Fatalf("old agent run: %s %q", r.Status, r.Message)
	}

	runID, _ := e.store.QueueBackup(ctx, j.ID, "manual")
	run := poll("0.33.7")
	if run == nil || run.ID != runID || run.ImageDisk != -1 {
		t.Fatalf("run for the agent: %+v", run)
	}
	details := `[{"number":0,"model":"System SSD","size":68719476736,"style":"gpt","partitions":[{"number":3,"offset":227540992,"length":67554508800,"mount_points":["C:\\"],"file_system":"NTFS","included":true,"method":"used-blocks","source":"vss","stored_bytes":2000}]},
	  {"number":1,"model":"Data HDD","size":1099511627776,"style":"gpt","partitions":[{"number":2,"offset":1048576,"length":1099510579200,"mount_points":["D:\\"],"file_system":"NTFS","included":true,"method":"used-blocks","source":"vss","stored_bytes":3000}]}]`
	if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("cd", 32),
		Stats: json.RawMessage(`{"bytes_read":5000}`), Details: json.RawMessage(details)}); err != nil {
		t.Fatal(err)
	}
	_, _, page := admin.do("GET", fmt.Sprintf("/runs/%d", runID), nil)
	for _, want := range []string{"System SSD", "Data HDD", "browse?part=3", "browse?part=1002", "Disk of the backup"} {
		if !strings.Contains(page, want) {
			t.Errorf("run page lacks %q", want)
		}
	}

	// Restore disk 1 of the backup (index 1) onto the agent's USB disk.
	_, loc, _ = admin.do("POST", fmt.Sprintf("/runs/%d/restore", runID), url.Values{"kind": {"image"}, "agent_id": {fmt.Sprint(a.ID)},
		"image_index": {"1"}, "target_disk": {"2"}, "confirm_erase": {"on"}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("restore: %s", loc)
	}
	rr := poll("0.33.7")
	if rr == nil || rr.Kind != api.KindImageRestore || rr.ImageIndex != 1 || rr.TargetDisk != 2 {
		t.Fatalf("restore run: %+v", rr)
	}
	if _, err := e.store.QueueImageRestore(ctx, runID, a.ID, server.ImageRestoreOptions{Image: 5, TargetDisk: 2}); err == nil {
		t.Error("restore of a disk the backup does not have accepted")
	}
}
