package server_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/server"
)

// A NAS job stores the share password encrypted, hands the share with the
// relative folders to the agent, keeps the password when the form leaves it
// empty and is listed with its share.
func TestNASJob(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "store", Kind: "local", URL: t.TempDir()})
	form := url.Values{"name": {"office nas"}, "kind": {"nas"}, "agent_id": {fmt.Sprint(agents[0].ID)}, "target_id": {fmt.Sprint(target)},
		"nas_url": {"smb://nas01/data"}, "nas_user": {"backup"}, "nas_domain": {"OFFICE"}, "nas_password": {"s3cret!pw"},
		"paths": {"Projects\n../etc"}, "sched_kind": {"manual"}, "keep_last": {"5"}}
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "err=") {
		t.Fatalf("folder outside the share accepted: %s", loc)
	}
	form.Set("nas_url", "ftp://nas01/data")
	form.Set("paths", "Projects\n\\Scans\\2026\\")
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "err=") {
		t.Fatalf("ftp share accepted: %s", loc)
	}
	form.Set("nas_url", "smb://nas01/data")
	_, loc, _ := admin.do("POST", "/jobs", form)
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("create: %s", loc)
	}
	var id int64
	fmt.Sscanf(strings.TrimPrefix(loc, "/jobs/"), "%d", &id)
	var stored string
	e.pool.QueryRow(ctx, `SELECT options->>'nas_password' FROM jobs WHERE id=$1`, id).Scan(&stored)
	if stored == "" || strings.Contains(stored, "s3cret") {
		t.Errorf("password stored as %q", stored)
	}
	share := func() *api.Run {
		t.Helper()
		rid, err := e.store.QueueBackup(ctx, id, "manual")
		if err != nil {
			t.Fatal(err)
		}
		ar, err := e.srv.APIRun(ctx, rid)
		if err != nil {
			t.Fatal(err)
		}
		e.pool.Exec(ctx, `UPDATE runs SET status='failed' WHERE id=$1`, rid)
		return ar
	}
	ar := share()
	if ar.Kind != api.KindBackup || ar.NAS == nil || ar.NAS.URL != "smb://nas01/data" || ar.NAS.User != "backup" || ar.NAS.Domain != "OFFICE" ||
		ar.NAS.Password != "s3cret!pw" || strings.Join(ar.Paths, "|") != "Projects|Scans/2026" {
		t.Fatalf("agent gets %+v %+v", ar, ar.NAS)
	}
	// Editing without a password keeps it.
	edit := url.Values{"name": {"office nas"}, "nas_url": {"smb://nas01/data"}, "nas_user": {"backup"}, "nas_domain": {"OFFICE"},
		"paths": {"Projects"}, "sched_kind": {"manual"}, "keep_last": {"5"}}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/edit", id), edit); !strings.Contains(loc, "msg=") {
		t.Fatalf("edit: %s", loc)
	}
	if ar := share(); ar.NAS.Password != "s3cret!pw" || strings.Join(ar.Paths, "|") != "Projects" {
		t.Errorf("after edit: %+v", ar.NAS)
	}
	_, _, body := admin.do("GET", "/jobs", nil)
	if !strings.Contains(body, `\\nas01\data`) || !strings.Contains(body, "NAS shares") {
		t.Error("job list lacks the share")
	}
}

// With a real SMB share (BACKUPZIT_TEST_SMB=smb://user@host/share and
// BACKUPZIT_TEST_SMB_PASSWORD) the agent backs up a folder of the share and
// restores it into a local folder.
func TestNASJobSMBLive(t *testing.T) {
	loc := os.Getenv("BACKUPZIT_TEST_SMB")
	if loc == "" {
		t.Skip("BACKUPZIT_TEST_SMB not set")
	}
	u, _ := url.Parse(loc)
	pw := os.Getenv("BACKUPZIT_TEST_SMB_PASSWORD")
	// Put a test folder on the share.
	be, err := backend.Open(context.Background(), loc, backend.Options{SMBPassword: pw})
	if err != nil {
		t.Fatal(err)
	}
	folder := fmt.Sprintf("nas-test-%d", time.Now().UnixNano())
	be.Save(context.Background(), folder+"/docs/hello.txt", []byte("hello from the NAS"))
	t.Cleanup(func() { be.Remove(context.Background(), folder+"/docs/hello.txt"); be.Close() })

	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "store", Kind: "local", URL: t.TempDir()})
	id, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobNAS, AgentID: agents[0].ID, TargetID: target, Name: "nas", Enabled: true,
		Paths: []string{folder}, Options: server.JobOptions{NASURL: "smb://" + u.Host + u.Path, NASUser: u.User.Username(), NASPassword: pw}})
	if err != nil {
		t.Fatal(err)
	}
	rid, _ := e.store.QueueBackup(ctx, id, "manual")
	runAgent(t, ag)
	run, _ := e.store.GetRun(ctx, rid)
	if run.Status != api.StatusSuccess || run.SnapshotID == "" {
		t.Fatalf("backup: %s %s %v", run.Status, run.Message, run.Errors)
	}
	dest := t.TempDir()
	if _, err := e.store.QueueRestore(ctx, rid, agents[0].ID, dest, nil, false); err != nil {
		t.Fatal(err)
	}
	runAgent(t, ag)
	var found string
	filepath.Walk(dest, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Name() == "hello.txt" {
			b, _ := os.ReadFile(p)
			found = string(b)
		}
		return nil
	})
	if found != "hello from the NAS" {
		t.Errorf("restored %q", found)
	}
}
