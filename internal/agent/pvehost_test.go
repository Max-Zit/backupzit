package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPVEConfigCopy(t *testing.T) {
	dir := t.TempDir()
	db := filepath.ToSlash(filepath.Join(dir, "pve-cluster", "config.db"))
	cp := filepath.ToSlash(filepath.Join(dir, "backupzit", "pve-config"))
	oldDB, oldDir, oldOS := pveConfigDB, pveConfigCopyDir, pveHostOS
	pveConfigDB, pveConfigCopyDir, pveHostOS = db, cp, true
	t.Cleanup(func() { pveConfigDB, pveConfigCopyDir, pveHostOS = oldDB, oldDir, oldOS })
	a := &Agent{log: slog.New(slog.DiscardHandler)}

	// Not a Proxmox node: paths unchanged.
	if got := a.withPVEConfigCopy([]string{"/"}); len(got) != 1 {
		t.Fatalf("no database: %v", got)
	}
	os.MkdirAll(filepath.Dir(db), 0o755)
	os.WriteFile(db, []byte("sqlite data"), 0o600)
	os.WriteFile(db+"-wal", []byte("wal"), 0o600)

	if got := a.withPVEConfigCopy([]string{"/etc"}); !slices.Equal(got, []string{"/etc"}) {
		t.Fatalf("database not included but copied: %v", got)
	}
	got := a.withPVEConfigCopy([]string{"/etc", filepath.Dir(db)})
	if !slices.Equal(got, []string{"/etc", filepath.Dir(db), cp}) {
		t.Fatalf("paths: %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(cp, "config.db")); string(b) != "sqlite data" {
		t.Fatalf("copy: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(cp, "config.db-wal")); string(b) != "wal" {
		t.Fatalf("wal copy: %q", b)
	}
	// The write-ahead log was checkpointed meanwhile: no stale copy of it.
	os.Remove(db + "-wal")
	a.withPVEConfigCopy([]string{db})
	if _, err := os.Stat(filepath.Join(cp, "config.db-wal")); err == nil {
		t.Fatal("stale wal copy kept")
	}
	// A path above both is not added twice.
	if got := a.withPVEConfigCopy([]string{dir}); len(got) != 1 {
		t.Fatalf("copy added below a covered path: %v", got)
	}
}

func TestCoversPath(t *testing.T) {
	for _, c := range []struct {
		paths []string
		want  bool
	}{
		{[]string{"/"}, true},
		{[]string{"/var/lib/pve-cluster"}, true},
		{[]string{"/var/lib/pve-cluster/"}, true},
		{[]string{"/var/lib"}, true},
		{[]string{"/var/lib/pve-cluster/config.db"}, true},
		{[]string{"/var/lib/pve"}, false},
		{[]string{"/etc", "/root"}, false},
	} {
		if got := coversPath(c.paths, "/var/lib/pve-cluster/config.db"); got != c.want {
			t.Errorf("%v: %v", c.paths, got)
		}
	}
}
