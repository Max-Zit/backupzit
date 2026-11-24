package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/testutil"
)

// #63: the agent tests storage the console cannot reach, with the run's
// credentials and the console's steps; a hardened repository is only
// listed, and a failed step comes back as the message.
func TestTargetTestRun(t *testing.T) {
	ctx := context.Background()
	a := New(&Config{ServerURL: "https://console.invalid", AgentUUID: "u", Secret: "s"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "0.60.0")

	srv, err := testutil.StartSFTPServer("backup", "nas-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rs := api.Repository{URL: srv.URL("site/backups"), SFTPPassword: "nas-pass-1", SFTPHostKey: srv.Fingerprint}
	res := a.targetTest(ctx, api.Run{Kind: api.KindTargetTest, Repository: rs})
	if res.Status != api.StatusSuccess || res.Message != "connected, wrote, read and removed a test file" {
		t.Fatalf("test: %s %s", res.Status, res.Message)
	}
	var d api.TargetTest
	if err := json.Unmarshal(res.Details, &d); err != nil || d.Immutable {
		t.Fatalf("details %s: %v", res.Details, err)
	}
	be, err := backend.Open(ctx, rs.URL, backend.Options{SFTPPassword: "nas-pass-1", SFTPHostKey: srv.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if names, err := be.List(ctx, ""); err != nil || len(names) != 0 {
		t.Errorf("left on the storage: %v %v", names, err)
	}
	be.Close()

	// Without the right password the step that failed is the message, with
	// no hint of the agent's own (the console adds its hints).
	bad := rs
	bad.SFTPPassword = "wrong"
	res = a.targetTest(ctx, api.Run{Kind: api.KindTargetTest, Repository: bad})
	if res.Status != api.StatusFailed || !strings.HasPrefix(res.Message, "connect: ") || strings.Contains(res.Message, "(the storage may be full") {
		t.Fatalf("wrong password: %s %s", res.Status, res.Message)
	}

	// A hardened repository keeps what is written: only a listing.
	h, err := testutil.StartHardened(filepath.Join(t.TempDir(), "repo"), 7)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	hr := api.Repository{URL: "hardened://" + h.Host + "/office", HardenedKey: h.Key, HardenedFingerprint: h.Fingerprint}
	res = a.targetTest(ctx, api.Run{Kind: api.KindTargetTest, Repository: hr})
	if res.Status != api.StatusSuccess || !strings.Contains(res.Message, "nothing written") {
		t.Fatalf("hardened: %s %s", res.Status, res.Message)
	}
	if err := json.Unmarshal(res.Details, &d); err != nil || !d.Immutable {
		t.Fatalf("hardened details %s: %v", res.Details, err)
	}
	// Storage the agent cannot open fails at connecting.
	if got := a.targetTest(ctx, api.Run{Kind: api.KindTargetTest, Repository: api.Repository{URL: "ftp://nowhere/x"}}); got.Status != api.StatusFailed || !strings.HasPrefix(got.Message, "connect: ") {
		t.Fatalf("unsupported storage: %+v", got)
	}
}

// #63: Find backups on network storage reachable by agents only signs in
// with the run's credentials (before 0.60 the agent opened the storage
// without any and could search only its own disks).
func TestRepoScanNetworkStorage(t *testing.T) {
	ctx := context.Background()
	a := New(&Config{ServerURL: "https://console.invalid", AgentUUID: "u", Secret: "s"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "0.60.0")
	srv, err := testutil.StartSFTPServer("backup", "nas-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rs := api.Repository{URL: srv.URL("site/backups"), SFTPPassword: "nas-pass-1", SFTPHostKey: srv.Fingerprint, Password: "recovery-key-1"}
	old := rs
	old.URL = joinLocation(rs.URL, "old-pc_1a2b")
	_, closeRepo, err := a.openRepo(ctx, old, true)
	if err != nil {
		t.Fatal(err)
	}
	closeRepo()
	res := a.repoScan(ctx, api.Run{Kind: api.KindRepoScan, Repository: rs})
	if res.Status != api.StatusSuccess {
		t.Fatalf("scan: %s", res.Message)
	}
	var scan api.RepoScan
	if err := json.Unmarshal(res.Details, &scan); err != nil || scan.Base != rs.URL || len(scan.Repos) != 1 || scan.Repos[0].Dir != "old-pc_1a2b" || scan.Repos[0].Error != "" {
		t.Fatalf("found %s: %v", res.Details, err)
	}
	rs.SFTPPassword = "wrong"
	if res := a.repoScan(ctx, api.Run{Kind: api.KindRepoScan, Repository: rs}); res.Status != api.StatusFailed {
		t.Fatal("searched with a wrong password")
	}
}
