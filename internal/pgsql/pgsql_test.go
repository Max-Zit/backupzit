package pgsql

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// P4 (0.55 review): a database name is never read as a connection string.
func TestDbnameArg(t *testing.T) {
	for name, want := range map[string]string{
		"shop":                      `--dbname=dbname='shop'`,
		"host=203.0.113.5 dbname=x": `--dbname=dbname='host=203.0.113.5 dbname=x'`,
		"postgresql://evil/x":       `--dbname=dbname='postgresql://evil/x'`,
		`it's`:                      `--dbname=dbname='it\'s'`,
		`back\slash' host=x`:        `--dbname=dbname='back\\slash\' host=x'`,
		"--file=/etc/passwd":        `--dbname=dbname='--file=/etc/passwd'`,
		"":                          `--dbname=dbname=''`,
		"Ünïcödé db":                `--dbname=dbname='Ünïcödé db'`,
	} {
		if got := dbnameArg(name); got != want {
			t.Errorf("dbnameArg(%q) = %s, want %s", name, got, want)
		}
	}
}

// P5 (0.55 review): the archive_command syncs the copy before the rename,
// accepts a segment that is already there only when it is identical, and
// BackupZit's older commands are recognized and upgraded.
func TestArchiveCommand(t *testing.T) {
	if strings.Contains(ArchiveCommand, "|") || strings.Contains(ArchiveCommand, "'") {
		t.Fatal("ArchiveState splits settings at | and ALTER SYSTEM quotes with '")
	}
	if !ownArchiveCommand(ArchiveCommand) || ownArchiveCommand("cp %p /mnt/wal/%f") {
		t.Fatal("ownArchiveCommand")
	}
	for _, c := range olderArchiveCommands {
		if !ownArchiveCommand(c) || c == ArchiveCommand {
			t.Fatalf("older command %q", c)
		}
	}
	if _, err := exec.LookPath("sh"); err != nil || runtime.GOOS == "windows" {
		t.Skip("archive_command runs in /bin/sh on Linux")
	}
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	if err := os.Mkdir(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	seg := "000000010000000000000003"
	src := filepath.Join(dir, seg)
	if err := os.WriteFile(src, []byte(strings.Repeat("wal", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() error {
		c := strings.NewReplacer(WALSpool, spool, "%p", src, "%f", seg).Replace(ArchiveCommand)
		return exec.Command("sh", "-c", c).Run() // #nosec G204 -- test command
	}
	if err := run(); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(spool, seg)); err != nil || len(b) != 3000 {
		t.Fatalf("archived copy: %d bytes, %v", len(b), err)
	}
	if _, err := os.Stat(filepath.Join(spool, seg+".part")); err == nil {
		t.Fatal("the temporary copy was left behind")
	}
	if err := run(); err != nil {
		t.Fatalf("archiving the same segment again (after a crash) must succeed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(spool, seg), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("a different file under the segment's name was accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(spool, seg)); string(b) != "short" {
		t.Fatal("an existing file was overwritten")
	}
}

// P5 (0.55 review): a short segment in the spool is removed only when
// nothing can still be writing it; otherwise it waits or is reported.
func TestJudgePartial(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	seg := "000000010000000000000010"
	old, fresh := now.Add(-time.Hour), now.Add(-10*time.Second)
	stuck := archiverState{lastArchived: "00000001000000000000000F", lastFailed: seg, failedAt: now.Add(-30 * time.Second), stuck: true}
	for _, c := range []struct {
		what  string
		mtime time.Time
		st    archiverState
		want  partialVerdict
	}{
		{"stale copy PostgreSQL fails on", old, stuck, partialRetry},
		{"copy written after the last failure", now.Add(-10 * time.Minute), archiverState{lastFailed: seg, failedAt: now.Add(-20 * time.Minute), stuck: true}, partialBlocked},
		{"fresh copy PostgreSQL fails on", fresh, stuck, partialBlocked},
		{"failure followed by a success", old, archiverState{lastArchived: "00000001000000000000000F", lastFailed: seg, failedAt: now.Add(-30 * time.Second)}, partialWait},
		{"copy in progress", fresh, archiverState{lastArchived: "00000001000000000000000F"}, partialWait},
		{"later segments archived", old, archiverState{lastArchived: "000000010000000000000012"}, partialLost},
		{"later segments archived, fresh file", fresh, archiverState{lastArchived: "000000010000000000000012"}, partialWait},
		{"later backup history file", old, archiverState{lastArchived: "000000010000000000000011.00000028.backup"}, partialLost},
		{"backup history file of the same segment", old, archiverState{lastArchived: seg + ".00000028.backup"}, partialWait},
		{"other timeline", old, archiverState{lastArchived: "000000020000000000000012"}, partialWait},
		{"history file", old, archiverState{lastArchived: "00000002.history"}, partialWait},
		{"nothing archived yet", old, archiverState{}, partialWait},
	} {
		if got := judgePartial(seg, c.mtime, now, c.st); got != c.want {
			t.Errorf("%s: %d, want %d", c.what, got, c.want)
		}
	}
}

// A complete segment is never removed, also one of another segment size;
// a short copy is, once PostgreSQL has moved past it.
func TestHandlePartial(t *testing.T) {
	dir := t.TempDir()
	const segBytes = 16 << 20
	now := time.Now()
	st := archiverState{lastArchived: "000000010000000000000020"}
	write := func(name string, b []byte) (string, os.FileInfo) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return p, fi
	}
	// A complete 1 MiB segment (its header says so) from a cluster with
	// another segment size.
	whole := make([]byte, 1<<20)
	binary.LittleEndian.PutUint32(whole[32:], 1<<20)
	p, fi := write("000000010000000000000011", whole)
	if msg := handlePartial(p, fi, segBytes, st, now); msg != "" {
		t.Fatalf("a complete segment was judged partial: %s", msg)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("a complete segment was removed")
	}
	// A short copy (header of a 16 MiB segment, 1 MiB of data).
	short := make([]byte, 1<<20)
	binary.LittleEndian.PutUint32(short[32:], segBytes)
	p, fi = write("000000010000000000000012", short)
	if msg := handlePartial(p, fi, segBytes, st, now); !strings.Contains(msg, "removed") {
		t.Fatalf("a stale short copy: %q", msg)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the stale short copy is still there")
	}
	// An empty file (power loss after the rename).
	p, fi = write("000000010000000000000013", nil)
	stuck := archiverState{lastArchived: "000000010000000000000012", lastFailed: "000000010000000000000013", failedAt: now, stuck: true}
	if msg := handlePartial(p, fi, segBytes, stuck, now); !strings.Contains(msg, "archives the segment again") {
		t.Fatalf("an empty copy that blocks archiving: %q", msg)
	}
}
