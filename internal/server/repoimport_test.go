package server_test

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/server"
)

// Backups on a storage target that the console does not know (e.g. made by
// an earlier console) are found, added as a removed agent's history and can
// be restored; adding them twice adds nothing.
func TestTargetScanImport(t *testing.T) {
	x := newRemoveEnv(t, "0.42.9")
	ctx := x.e.ctx
	targets, err := x.e.store.ListTargets(ctx)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %v %v", targets, err)
	}
	tg := targets[0]

	// Another console's agent left two backups of one job in old-pc_1a2b3c4d.
	u, _ := url.Parse(tg.URL)
	u.Path = path.Join(u.Path, "old-pc_1a2b3c4d")
	be, err := backend.Open(ctx, u.String(),
		backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password(""))
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for i := 0; i < 2; i++ {
		os.WriteFile(filepath.Join(src, "a.txt"), []byte(fmt.Sprint("old ", i)), 0o644)
		if _, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Hostname: "OLD-PC", Tags: []string{"job:7"}}); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()

	page := func(p string) string {
		code, _, body := x.admin.do("GET", p, nil)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		return body
	}
	scan := fmt.Sprintf("/targets/%d/scan", tg.ID)
	b := page(scan)
	if !strings.Contains(b, `value="old-pc_1a2b3c4d"`) || !strings.Contains(b, "OLD-PC") || !strings.Contains(b, "<td>2</td>") {
		t.Fatalf("unknown repository not listed:\n%s", b)
	}
	if own, _ := url.Parse(x.repoURL); strings.Contains(b, `value="`+path.Base(own.Path)+`"`) {
		t.Fatal("the agent's own repository is listed as unknown")
	}
	if !strings.Contains(b, "Repositories on this storage already in the console: 1.") {
		t.Fatal("known repository not counted")
	}

	code, loc, _ := x.admin.do("POST", fmt.Sprintf("/targets/%d/import", tg.ID), url.Values{"dir": {"old-pc_1a2b3c4d"}})
	if code != 303 || !strings.Contains(loc, "Backups+added%3A+2%2C+of+computers%3A+1") {
		t.Fatalf("import: %d %s", code, loc)
	}
	removed := 0
	var oldID int64
	agents, _ := x.e.store.ListAgents(ctx)
	if len(agents) != 1 {
		t.Fatalf("imported agent is active: %d agents", len(agents))
	}
	if b := page("/agents"); !strings.Contains(b, "OLD-PC") {
		t.Fatal("imported backups not listed under removed agents")
	}
	if b := page("/runs"); strings.Count(b, "OLD-PC") != 2 || !strings.Contains(b, "Files "+src) {
		t.Fatalf("imported runs not in Activity")
	}
	runs, err := x.e.store.ListRuns(ctx, server.RunFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var newest int64
	for _, run := range runs {
		if run.Hostname == "OLD-PC" {
			removed++
			oldID = run.AgentID
			if run.Trigger != "imported" || run.Status != "success" || run.SnapshotID == "" || !run.AgentRemoved {
				t.Fatalf("imported run: %+v", run)
			}
			if run.ID > newest {
				newest = run.ID
			}
		}
	}
	if removed != 2 || oldID == 0 {
		t.Fatalf("imported runs: %d", removed)
	}

	// The backups restore to the current agent.
	if _, err := x.e.store.QueueRestore(ctx, newest, x.agentID, t.TempDir(), nil, false); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// A second search finds nothing new; a repeated import adds nothing.
	if b := page(scan); strings.Contains(b, `value="old-pc_1a2b3c4d"`) || !strings.Contains(b, "No unknown backups found.") {
		t.Fatal("imported repository still unknown")
	}
	x.admin.do("POST", fmt.Sprintf("/targets/%d/import", tg.ID), url.Values{"dir": {"old-pc_1a2b3c4d"}})
	runs, _ = x.e.store.ListRuns(ctx, server.RunFilter{Limit: 500})
	n := 0
	for _, run := range runs {
		if run.Hostname == "OLD-PC" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("repeated import: %d runs", n)
	}
	// Names that are not folders are refused.
	x.admin.do("POST", fmt.Sprintf("/targets/%d/import", tg.ID), url.Values{"dir": {"../x", "backupzit-console"}})
	if after, _ := x.e.store.ListRuns(ctx, server.RunFilter{Limit: 500}); len(after) != len(runs) {
		t.Fatal("invalid folder imported")
	}
}

// Old backups of a computer whose current agent has backups of its own are
// connected as history only: the agent keeps its repositories.
func TestImportConnectKeepsOwnRepo(t *testing.T) {
	x := newRemoveEnv(t, "0.42.9")
	ctx := x.e.ctx
	targets, _ := x.e.store.ListTargets(ctx)
	tg := targets[0]
	u, _ := url.Parse(tg.URL)
	u.Path = path.Join(u.Path, "pc-17_00c0ffee")
	be, err := backend.Open(ctx, u.String(), backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password(""))
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "old.txt"), []byte("old"), 0o644)
	if _, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Hostname: x.host}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if code, loc, _ := x.admin.do("POST", fmt.Sprintf("/targets/%d/import", tg.ID), url.Values{"dir": {"pc-17_00c0ffee"}}); code != 303 || !strings.Contains(loc, "Backups+added%3A+1") {
		t.Fatalf("import: %d %s", code, loc)
	}
	before, _ := x.e.store.GetAgent(ctx, x.agentID)
	var removedID int64
	runs, _ := x.e.store.ListRuns(ctx, server.RunFilter{Limit: 500})
	for _, run := range runs {
		if run.AgentRemoved {
			removedID = run.AgentID
		}
	}
	if removedID == 0 {
		t.Fatal("no imported run")
	}
	if b := func() string { _, _, b := x.admin.do("GET", "/agents", nil); return b }(); !strings.Contains(b, fmt.Sprintf(`action="/agents/%d/adopt"`, x.agentID)) {
		t.Fatal("Connect not offered")
	}
	code, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/adopt", x.agentID), url.Values{"removed": {fmt.Sprint(removedID)}})
	if code != 303 || !strings.Contains(loc, "keep+their+own+repositories") {
		t.Fatalf("adopt: %d %s", code, loc)
	}
	after, _ := x.e.store.GetAgent(ctx, x.agentID)
	if after.RepoDir != before.RepoDir {
		t.Fatalf("repository changed: %s -> %s", before.RepoDir, after.RepoDir)
	}
	runs, _ = x.e.store.ListRuns(ctx, server.RunFilter{AgentID: x.agentID, Limit: 500})
	if len(runs) != 4 {
		t.Fatalf("agent has %d runs, want 3 own + 1 connected", len(runs))
	}
}
