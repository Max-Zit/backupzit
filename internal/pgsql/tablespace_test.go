package pgsql

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// H-5 (0.55): a recovered cluster must not use the original tablespace
// paths (the live cluster's data): the tablespaces are unpacked into the
// work folder and tablespace_map points there.
func TestMapTablespaces(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil || runtime.GOOS == "windows" {
		t.Skip("PostgreSQL restores run on Linux agents (GNU tar)")
	}
	work := t.TempDir()
	base := filepath.Join(work, "base")
	data := filepath.Join(work, "data")
	for _, d := range []string{base, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Create(filepath.Join(base, "16385.tar"))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	body := []byte("table data")
	_ = tw.WriteHeader(&tar.Header{Name: "PG_16_202307071/5/16390", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = f.Close()
	if err := os.WriteFile(filepath.Join(data, "tablespace_map"), []byte("16385 /mnt/live tablespace\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(work, "tablespaces")
	if err := mapTablespaces(context.Background(), data, base, dir); err != nil {
		t.Fatal(err)
	}
	m, _ := os.ReadFile(filepath.Join(data, "tablespace_map"))
	if strings.Contains(string(m), "/mnt/live") || strings.TrimSpace(string(m)) != "16385 "+filepath.Join(dir, "16385") {
		t.Fatalf("tablespace_map: %q", m)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "16385", "PG_16_202307071", "5", "16390")); err != nil || string(b) != "table data" {
		t.Fatalf("tablespace not unpacked: %v %q", err, b)
	}

	// A tablespace whose tar is missing refuses the restore.
	_ = os.WriteFile(filepath.Join(data, "tablespace_map"), []byte("16999 /mnt/other\n"), 0o600)
	if err := mapTablespaces(context.Background(), data, base, dir); err == nil || !strings.Contains(err.Error(), "16999") {
		t.Fatalf("missing tablespace accepted: %v", err)
	}
	// No tablespace_map: nothing to do.
	_ = os.Remove(filepath.Join(data, "tablespace_map"))
	if err := mapTablespaces(context.Background(), data, base, dir); err != nil {
		t.Fatal(err)
	}
}
