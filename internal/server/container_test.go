package server_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/max-zit/backupzit/internal/server"
)

// In the container image a new version copies the console before it
// changes the database; the copy brings the console back to that state.
func TestPreUpgradeCopy(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().Port(port).
		RuntimePath(filepath.Join(dir, "pg")).Logger(io.Discard))
	if err := pg.Start(); err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { pg.Stop() })
	ctx := context.Background()
	url := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
	data := filepath.Join(dir, "data")
	os.MkdirAll(data, 0o700)
	version := func() string {
		b, _ := os.ReadFile(filepath.Join(data, "version"))
		return strings.TrimSpace(string(b))
	}

	// A new installation: nothing to copy.
	if d, _, err := server.PreUpgradeCopy(ctx, url, data, "0.45.3"); err != nil || d != "" || version() != "0.45.3" {
		t.Fatalf("new installation: %q %v %q", d, err, version())
	}
	pool, err := server.OpenDB(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	store := server.NewStore(pool)
	key, _, err := server.LoadOrCreateSecretKey(data)
	if err != nil {
		t.Fatal(err)
	}
	store.UseSecretKey(key)
	if _, err := store.CreateTarget(ctx, server.Target{Name: "before", Kind: "local", URL: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	pool.Close()

	// The same version again: no copy. A new version: one copy, once.
	if d, _, _ := server.PreUpgradeCopy(ctx, url, data, "0.45.3"); d != "" {
		t.Fatal("copy without a new version")
	}
	copyDir, from, err := server.PreUpgradeCopy(ctx, url, data, "0.46.0")
	if err != nil || copyDir == "" || from != "0.45.3" || version() != "0.46.0" {
		t.Fatalf("new version: %q %q %v %q", copyDir, from, err, version())
	}
	if _, err := os.Stat(filepath.Join(copyDir, "files", server.SecretKeyFile)); err != nil {
		t.Fatalf("secrets key not in the copy: %v", err)
	}
	if d, _, _ := server.PreUpgradeCopy(ctx, url, data, "0.46.0"); d != "" {
		t.Fatal("second copy for the same version")
	}

	// The new version changes data; going back restores the copy.
	pool, _ = server.OpenDB(ctx, url)
	store = server.NewStore(pool)
	store.UseSecretKey(key)
	store.CreateTarget(ctx, server.Target{Name: "after", Kind: "local", URL: t.TempDir()})
	pool.Close()
	if err := server.RestorePreUpgrade(ctx, url, data, copyDir, t.Logf); err != nil {
		t.Fatal(err)
	}
	pool, _ = server.OpenDB(ctx, url)
	defer pool.Close()
	store = server.NewStore(pool)
	store.UseSecretKey(key)
	targets, _ := store.ListTargets(ctx)
	if len(targets) != 1 || targets[0].Name != "before" {
		t.Fatalf("targets after going back: %+v", targets)
	}
	if version() != "0.45.3" {
		t.Fatalf("version after going back: %q", version())
	}

	// Only the newest three copies are kept.
	for _, v := range []string{"0.47.0", "0.48.0", "0.49.0"} {
		if _, _, err := server.PreUpgradeCopy(ctx, url, data, v); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(data, server.PreUpgradeDir))
	if len(entries) != 3 {
		t.Fatalf("%d copies kept", len(entries))
	}
}

// In the container the Updates page explains docker compose instead of
// offering to install from the console.
func TestUpdatesPageInContainer(t *testing.T) {
	t.Setenv("BACKUPZIT_CONTAINER", "docker")
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	code, _, body := admin.do("GET", "/settings/updates", nil)
	if code != 200 || !strings.Contains(body, "This console runs in a container") || strings.Contains(body, "Updates from the console need the Linux package") {
		t.Fatalf("updates page in a container: %d", code)
	}
}
