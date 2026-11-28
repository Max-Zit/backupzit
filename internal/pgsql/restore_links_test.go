//go:build !windows

package pgsql

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tarEntry is one member of a test archive.
type tarEntry struct {
	name, link, body string
	typ              byte
	mode             int64
}

func writeTar(t *testing.T, p string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.body)), ModTime: time.Now()}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func needTar(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("PostgreSQL restores run on Linux agents (GNU tar)")
	}
}

// restoreDirs creates the folders of a restore: work, the fetched base
// backup, the WAL folder, and a file only root may change outside them.
func restoreDirs(t *testing.T) (work, base, walDir, secret string) {
	t.Helper()
	work = t.TempDir()
	base = filepath.Join(work, "base")
	walDir = filepath.Join(work, "wal")
	for _, d := range []string{base, walDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secret = filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(secret, []byte("root only"), 0o600); err != nil {
		t.Fatal(err)
	}
	return work, base, walDir, secret
}

// NEW-1 (0.63): postgres writes the cluster backup, so base.tar may hold
// links where root writes the recovered cluster's configuration
// (pg_ident.conf -> /etc/passwd made root empty that file). Root replaces
// them with its own files and writes nowhere else; the data folder stays
// private whatever mode the archive gives "."; setuid bits are not kept.
func TestPrepareDataDoesNotWriteThroughLinks(t *testing.T) {
	needTar(t)
	work, base, walDir, secret := restoreDirs(t)
	elsewhere := filepath.Join(filepath.Dir(secret), "auto.conf")
	entries := []tarEntry{
		{name: "./", typ: tar.TypeDir, mode: 0o777},
		{name: "PG_VERSION", typ: tar.TypeReg, mode: 0o600, body: "16\n"},
		{name: "global/", typ: tar.TypeDir, mode: 0o700},
		{name: "global/pg_control", typ: tar.TypeReg, mode: 0o4755, body: "control"},
		{name: "postgresql.auto.conf", typ: tar.TypeSymlink, link: elsewhere},
	}
	for _, n := range []string{"pg_ident.conf", "pg_hba.conf", "recovery.signal", "postgresql.conf"} {
		entries = append(entries, tarEntry{name: n, typ: tar.TypeSymlink, link: secret})
	}
	writeTar(t, filepath.Join(base, "base.tar"), entries)
	data := filepath.Join(work, "data")
	socket := filepath.Join(work, "run")
	if err := prepareData(context.Background(), data, base, filepath.Join(work, "tablespaces"), socket, walDir, 54330, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "root only" {
		t.Fatalf("written through a link of the archive: %q", b)
	}
	if _, err := os.Lstat(elsewhere); err == nil {
		t.Fatal("postgresql.auto.conf was written where its link pointed")
	}
	for name, want := range map[string]string{
		"postgresql.auto.conf": "unix_socket_directories = '" + socket + "'",
		"pg_hba.conf":          "local all all peer",
		"pg_ident.conf":        "",
		"recovery.signal":      "",
		"postgresql.conf":      "written by BackupZit",
	} {
		p := filepath.Join(data, name)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s: %v %v, want a regular file", name, fi, err)
			continue
		}
		if b, _ := os.ReadFile(p); !strings.Contains(string(b), want) || want == "" && len(b) > 0 {
			t.Errorf("%s: %q, want %q", name, b, want)
		}
	}
	if fi, err := os.Stat(data); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("data folder: %v, want 0700", fi.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Join(data, "global", "pg_control")); err != nil {
		t.Error(err)
	} else if fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("pg_control: %v, want no setuid bit", fi.Mode())
	}
}

// tar follows links on its way into a folder: pg_wal.tar is not unpacked
// into a pg_wal that base.tar made a link to another folder.
func TestPrepareDataRefusesLinkedPgWal(t *testing.T) {
	needTar(t)
	work, base, walDir, _ := restoreDirs(t)
	outside := t.TempDir()
	writeTar(t, filepath.Join(base, "base.tar"), []tarEntry{
		{name: "PG_VERSION", typ: tar.TypeReg, mode: 0o600, body: "16\n"},
		{name: "pg_wal", typ: tar.TypeSymlink, link: outside},
	})
	writeTar(t, filepath.Join(base, "pg_wal.tar"), []tarEntry{{name: "planted", typ: tar.TypeReg, mode: 0o600, body: "x"}})
	err := prepareData(context.Background(), filepath.Join(work, "data"), base, filepath.Join(work, "tablespaces"), filepath.Join(work, "run"), walDir, 54330, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("pg_wal as a link accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "planted")); err == nil {
		t.Fatal("pg_wal.tar was unpacked through the link")
	}
}

// A base.tar that is a link (to a file postgres cannot read) is not
// unpacked.
func TestPrepareDataRefusesLinkedArchive(t *testing.T) {
	work, base, walDir, secret := restoreDirs(t)
	if err := os.Symlink(secret, filepath.Join(base, "base.tar")); err != nil {
		t.Fatal(err)
	}
	err := prepareData(context.Background(), filepath.Join(work, "data"), base, filepath.Join(work, "tablespaces"), filepath.Join(work, "run"), walDir, 54330, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a linked base.tar: %v", err)
	}
}

// tablespace_map is read and replaced without following a link, and a
// tablespace listed twice is refused: its second unpack would go through
// the links the first one left in its folder.
func TestMapTablespacesRefusesLinksAndRepeats(t *testing.T) {
	work, base, _, secret := restoreDirs(t)
	data := filepath.Join(work, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(data, "tablespace_map")); err != nil {
		t.Fatal(err)
	}
	if err := mapTablespaces(context.Background(), data, base, filepath.Join(work, "tablespaces")); err == nil {
		t.Fatal("a linked tablespace_map was read")
	}
	if b, _ := os.ReadFile(secret); string(b) != "root only" {
		t.Fatalf("the link target changed: %q", b)
	}
	needTar(t)
	_ = os.Remove(filepath.Join(data, "tablespace_map"))
	writeTar(t, filepath.Join(base, "16385.tar"), []tarEntry{{name: "PG_16_202307071/", typ: tar.TypeDir, mode: 0o700}})
	if err := os.WriteFile(filepath.Join(data, "tablespace_map"), []byte("16385 /mnt/a\n16385 /mnt/b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mapTablespaces(context.Background(), data, base, filepath.Join(work, "tablespaces")); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a tablespace listed twice: %v", err)
	}
}

// putFile replaces a link at its name and leaves the link's target alone.
func TestPutFileReplacesLink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("root only"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "pg_ident.conf")
	if err := os.Symlink(secret, p); err != nil {
		t.Fatal(err)
	}
	if err := putFile(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "root only" {
		t.Errorf("the link target changed: %q", b)
	}
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%v %v, want a regular file", fi, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Errorf("content %q", b)
	}
}
