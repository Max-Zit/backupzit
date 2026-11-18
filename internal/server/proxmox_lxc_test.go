package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/runinfo"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

// TestProxmoxContainersOnZFS: a Proxmox backup with a VM (QEMU) and an LXC
// container on ZFS, whose rootfs is stored as files. The console lists VMs
// and containers apart, browses the container's files like a file backup,
// and offers only the storages each guest can be restored to.
func TestProxmoxContainersOnZFS(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/backups/pve?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.53.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	inv := `{"node":"pve","guests":[{"vmid":100,"name":"web","type":"qemu","node":"pve","status":"running","maxdisk":8589934592},
		{"vmid":200,"name":"storage02","type":"lxc","node":"pve","status":"running","maxdisk":4294967296}],
		"storage":["local-lvm","local-zfs"],
		"storage_info":[{"id":"local-lvm","type":"lvmthin","vms":true,"containers":true},{"id":"local-zfs","type":"zfspool","vms":true,"containers":true}]}`
	if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Hypervisor: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	jobID, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobVM, AgentID: a.ID, TargetID: targetID, Name: "Proxmox", Paths: []string{"100", "200"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	run, err := e.store.ClaimRun(ctx, a.ID)
	if err != nil || run == nil || run.ID != runID {
		t.Fatalf("claim: %+v %v", run, err)
	}

	// The snapshot as the agent writes it: the container's rootfs is the
	// directory lxc/200/rootfs of the tree.
	be, err := backend.Open(ctx, run.RepoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password(""))
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "etc"), 0o755)
	os.WriteFile(filepath.Join(src, "etc", "hostname"), []byte("storage02\n"), 0o644)
	rootfs, _, err := archiver.Dir(ctx, r, src, "rootfs", nil, archiver.Options{OneFileSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := func(name string, n repo.Node) repo.Node {
		id, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{n}})
		if err != nil {
			t.Fatal(err)
		}
		return repo.Node{Name: name, Type: repo.NodeDir, Mode: uint32(os.ModeDir | 0o755), Subtree: &id}
	}
	root := dir("", dir("lxc", dir("200", rootfs)))
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sn := &repo.Snapshot{Time: time.Now().UTC(), Hostname: "pve", Tree: *root.Subtree, Guests: []repo.Guest{
		{Platform: "proxmox", Type: "qemu", VMID: 100, Name: "web", Consistency: "snapshot, file systems frozen by the QEMU guest agent",
			Disks: []repo.GuestDisk{{Key: "scsi0", Volume: "local-lvm:vm-100-disk-0", Size: 8 << 30, Image: 0}}},
		{Platform: "proxmox", Type: "lxc", VMID: 200, Name: "storage02", Consistency: "snapshot of the running container",
			Disks: []repo.GuestDisk{{Key: "rootfs", Volume: "local-zfs:subvol-200-disk-0", Format: "subvol", Size: 4 << 30, Image: -1, Path: "lxc/200/rootfs", Bytes: 10}}},
	}}
	if _, err := r.SaveSnapshot(ctx, sn); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(),
		Stats: json.RawMessage(`{"files":2,"bytes_read":10}`), Details: runinfo.Guests(sn)}); err != nil {
		t.Fatal(err)
	}

	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	get := func(p string) string {
		t.Helper()
		code, _, body := admin.do("GET", p, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d", p, code)
		}
		return body
	}
	page := get(fmt.Sprintf("/runs/%d", runID))
	for _, want := range []string{
		"Proxmox virtual machines (QEMU)", "Proxmox containers (LXC)", "VMs backed up", "containers backed up",
		"files (ZFS subvolume)", fmt.Sprintf(`/runs/%d/files?path=lxc`, runID), fmt.Sprintf(`/runs/%d/vmbrowse?vmid=100`, runID),
		`data-type="lxc" data-files`, `data-type="qemu"`, `data-st="zfspool"`, "data-vm-only",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("run page lacks %q", want)
		}
	}
	if strings.Contains(page, fmt.Sprintf(`/runs/%d/vmbrowse?vmid=200`, runID)) {
		t.Error("container stored as files is browsed as a disk")
	}

	// Old links to the disk browser lead to the container's files.
	if code, loc, _ := admin.do("GET", fmt.Sprintf("/runs/%d/vmbrowse?vmid=200", runID), nil); code != http.StatusSeeOther || !strings.Contains(loc, "/files?path=lxc%2F200") {
		t.Errorf("vmbrowse of the container: %d %s", code, loc)
	}
	if page := get(fmt.Sprintf("/runs/%d/files?path=lxc/200/rootfs/etc", runID)); !strings.Contains(page, `value="lxc/200/rootfs/etc/hostname"`) {
		t.Error("container files not listed")
	}
	v := url.Values{"path": {"lxc/200/rootfs/etc"}, "sel": {"lxc/200/rootfs/etc/hostname"}}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/runs/%d/files-download", e.ts.URL, runID), strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err := admin.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "storage02\n" {
		t.Errorf("download: %q", body)
	}
	if _, err := e.store.QueueRestore(ctx, runID, a.ID, t.TempDir(), []string{"lxc/200/rootfs/etc"}, false); err != nil {
		t.Errorf("restoring container files on an agent: %v", err)
	}

	page = get(fmt.Sprintf("/jobs/%d", jobID))
	for _, want := range []string{"Protected VMs and containers", "container (LXC)", "as files", "VM (QEMU)", fmt.Sprintf(`/runs/%d/files?path=lxc`, runID)} {
		if !strings.Contains(page, want) {
			t.Errorf("job page lacks %q", want)
		}
	}
	page = get(fmt.Sprintf("/jobs/%d/edit", jobID))
	if !strings.Contains(page, "Virtual machines (QEMU) · 1") || !strings.Contains(page, "Containers (LXC) · 1") {
		t.Error("job form does not list VMs and containers apart")
	}
	page = get("/restore?type=vm")
	if !strings.Contains(page, "container (LXC) on") || !strings.Contains(page, "VM (QEMU) on") {
		t.Error("restore wizard does not tell VMs and containers apart")
	}
}
