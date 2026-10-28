package server_test

import (
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
	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/server"
)

// Backups on a local disk are found by the agent of the computer the disk
// is connected to, then added like those on network storage.
func TestLocalDiskScan(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.44.0")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "0.44.0")
	ag.VSS = false
	if _, err := ag.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(t.TempDir(), "usb-disk")
	id, err := e.store.CreateTarget(ctx, server.Target{Name: "disk", Kind: "local", URL: disk})
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := e.store.GetTarget(ctx, id)

	// An earlier installation left two backups in old-pc_1a2b, and one
	// encrypted with another key in other_9f.
	mk := func(dir, password string, n int) {
		be, err := backend.Open(ctx, filepath.Join(disk, dir), backend.Options{})
		if err != nil {
			t.Fatal(err)
		}
		r, err := repo.Init(ctx, be, repo.Password(password))
		if err != nil {
			t.Fatal(err)
		}
		src := t.TempDir()
		for i := 0; i < n; i++ {
			os.WriteFile(filepath.Join(src, "a.txt"), []byte(fmt.Sprint("old ", i)), 0o644)
			if _, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Hostname: "OLD-PC", Tags: []string{"job:7"}}); err != nil {
				t.Fatal(err)
			}
		}
		r.Close()
	}
	mk("old-pc_1a2b", tg.RecoveryKey, 2)
	mk("other_9f", "another key", 1)

	page := func(p string) string {
		code, _, body := admin.do("GET", p, nil)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		return body
	}
	scan := fmt.Sprintf("/targets/%d/scan", id)
	if b := page(scan); !strings.Contains(b, `name="agent_id"`) || strings.Contains(b, "could not be searched") {
		t.Fatalf("no agent choice:\n%s", b)
	}
	agents, _ := e.store.ListAgents(ctx)
	_, loc, _ := admin.do("POST", scan, url.Values{"agent_id": {fmt.Sprint(agents[0].ID)}})
	if !strings.Contains(loc, scan+"?run=") {
		t.Fatalf("search not started: %s", loc)
	}
	if b := page(loc); !strings.Contains(b, "is searching the disk") || !strings.Contains(b, "data-autorefresh") {
		t.Fatal("no waiting notice")
	}
	if _, err := ag.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ag.Wait()
	b := page(loc)
	if !strings.Contains(b, `value="old-pc_1a2b"`) || !strings.Contains(b, "OLD-PC") || !strings.Contains(b, "<td>2</td>") {
		t.Fatalf("found repository not listed:\n%s", b)
	}
	if !strings.Contains(b, "encrypted with another recovery key") || strings.Contains(b, `value="other_9f"`) {
		i := strings.Index(b, "other_9f")
		t.Fatalf("repository with another key not shown as unreadable: %.400s", b[max(i, 0):])
	}
	run := strings.TrimPrefix(loc, scan+"?run=")
	code, loc2, _ := admin.do("POST", fmt.Sprintf("/targets/%d/import", id), url.Values{"dir": {"old-pc_1a2b"}, "run": {run}})
	if code != 303 || !strings.Contains(loc2, "Backups+added%3A+2%2C+of+computers%3A+1") {
		t.Fatalf("import: %d %s", code, loc2)
	}
	// The added backups point at the folder on the disk.
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{Limit: 50})
	n := 0
	for _, r := range runs {
		if r.Trigger == "imported" {
			n++
			if want := strings.TrimRight(disk, `/\`) + "/old-pc_1a2b"; r.RepoURL != want {
				t.Errorf("repository %q, want %q", r.RepoURL, want)
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d imported runs", n)
	}
	// Searching again lists it as known.
	_, loc, _ = admin.do("POST", scan, url.Values{"agent_id": {fmt.Sprint(agents[0].ID)}})
	ag.PollOnce(ctx)
	ag.Wait()
	if b := page(loc); strings.Contains(b, `value="old-pc_1a2b"`) || !strings.Contains(b, "already in the console: 1.") {
		t.Fatal("added repository still listed as unknown")
	}
	// Without a finished search nothing is imported.
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/targets/%d/import", id), url.Values{"dir": {"old-pc_1a2b"}}); !strings.Contains(loc, "err=") {
		t.Fatal("import without a search")
	}
}
