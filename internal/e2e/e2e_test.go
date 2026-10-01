// Package e2e tests full backup and restore cycles against real backends.
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/backupzit/backupzit/internal/archiver"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/checker"
	"github.com/backupzit/backupzit/internal/fsutil"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
	"github.com/backupzit/backupzit/internal/testutil"
)

const mib = 1 << 20

// entry is what we compare between source and restore.
type entry struct {
	Type  string
	Size  int64
	Hash  string
	MTime time.Time
	Attrs uint32
}

func manifest(t *testing.T, root string) map[string]entry {
	t.Helper()
	m := map[string]entry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		e := entry{Attrs: fsutil.WinAttrs(fi)}
		switch {
		case fi.IsDir():
			e.Type = "dir"
		case fi.Mode().IsRegular():
			e.Type = "file"
			e.Size = fi.Size()
			e.MTime = fi.ModTime().UTC()
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
			e.Hash = hex.EncodeToString(h.Sum(nil))
		default:
			e.Type = "other"
		}
		m[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func compareManifests(t *testing.T, want, got map[string]entry) {
	t.Helper()
	bad := 0
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			t.Errorf("missing after restore: %s", p)
		case w.Type != g.Type || w.Size != g.Size || w.Hash != g.Hash:
			t.Errorf("content differs: %s: want %+v got %+v", p, w, g)
		case w.Type == "file" && !w.MTime.Equal(g.MTime):
			t.Errorf("mtime differs: %s: want %v got %v", p, w.MTime, g.MTime)
		case w.Attrs != g.Attrs:
			t.Errorf("attributes differ: %s: want %#x got %#x", p, w.Attrs, g.Attrs)
		default:
			continue
		}
		if bad++; bad > 20 {
			t.Fatal("too many differences")
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected entry after restore: %s", p)
		}
	}
}

func writeFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

// makeSource builds a test tree covering the cases we care about.
func makeSource(t *testing.T, root string, rng *rand.Rand) {
	writeFile(t, filepath.Join(root, "empty.txt"), nil)
	writeFile(t, filepath.Join(root, "one-byte.bin"), []byte{0x42})
	writeFile(t, filepath.Join(root, "small.txt"), []byte("hello backupzit\n"))
	writeFile(t, filepath.Join(root, "docs", "report 2026 (final).txt"), []byte(strings.Repeat("compressible text line\n", 200000)))
	writeFile(t, filepath.Join(root, "docs", "unicode čćžšđ 文件 ü.txt"), []byte("unicode name"))
	writeFile(t, filepath.Join(root, "media", "random-1m.bin"), randBytes(rng, 1*mib))
	writeFile(t, filepath.Join(root, "media", "random-30m.bin"), randBytes(rng, 30*mib))
	writeFile(t, filepath.Join(root, "media", "random-9m.bin"), randBytes(rng, 9*mib+123))
	if err := os.MkdirAll(filepath.Join(root, "empty-dir", "nested-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Many small files.
	for i := 0; i < 300; i++ {
		writeFile(t, filepath.Join(root, "many", fmt.Sprintf("d%02d", i%10), fmt.Sprintf("f%03d.dat", i)), randBytes(rng, rng.Intn(20000)))
	}
	// Deep path beyond the classic 260 character Windows limit.
	deep := root
	for i := 0; i < 12; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("very-long-directory-name-level-%02d", i))
	}
	writeFile(t, filepath.Join(deep, "deep-file.txt"), []byte("deep"))

	ro := filepath.Join(root, "readonly.txt")
	writeFile(t, ro, []byte("read only"))
	if err := os.Chmod(ro, 0o444); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		hidden := filepath.Join(root, "hidden.txt")
		writeFile(t, hidden, []byte("hidden"))
		if err := fsutil.SetWinAttrs(hidden, 0x2 /* HIDDEN */); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct, old mtime to make sure it is restored.
	old := time.Date(2001, 2, 3, 4, 5, 6, 700, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "small.txt"), old, old); err != nil {
		t.Fatal(err)
	}
}

// tempDir returns a temp dir whose read-only files are made writable before
// the testing package removes it (Windows refuses to delete them otherwise).
func tempDir(t *testing.T) string {
	d := t.TempDir()
	t.Cleanup(func() {
		filepath.WalkDir(d, func(p string, _ fs.DirEntry, err error) error {
			if err == nil {
				fsutil.ClearReadOnly(p)
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	return d
}

func restoredRoot(t *testing.T, target, src string) string {
	comps, err := fsutil.SnapshotComponents(src)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(append([]string{target}, comps...)...)
}

func TestBackupRestoreLocal(t *testing.T) {
	be, err := backend.OpenLocal(filepath.Join(tempDir(t), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	runCycle(t, func() backend.Backend { return be }, true)
}

func TestBackupRestoreSFTP(t *testing.T) {
	srv, err := testutil.StartSFTPServer("backup", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx := context.Background()

	// Unpinned host key must be refused and report the fingerprint.
	_, err = backend.Open(ctx, srv.URL("repo"), backend.Options{SFTPPassword: "s3cret"})
	if err == nil || !strings.Contains(err.Error(), srv.Fingerprint) {
		t.Fatalf("expected unpinned host key error with fingerprint, got %v", err)
	}
	// Wrong password must fail.
	if _, err := backend.Open(ctx, srv.URL("repo"), backend.Options{SFTPPassword: "nope", SFTPHostKey: srv.Fingerprint}); err == nil {
		t.Fatal("expected auth failure")
	}

	open := func() backend.Backend {
		be, err := backend.Open(ctx, srv.URL("repo"), backend.Options{SFTPPassword: "s3cret", SFTPHostKey: srv.Fingerprint})
		if err != nil {
			t.Fatal(err)
		}
		return be
	}
	runCycle(t, open, false)
}

// runCycle performs: init, full backup, restore+compare, modify, incremental
// backup, restore both snapshots, partial restore, check, and (for local
// repositories) corruption detection.
func runCycle(t *testing.T, openBE func() backend.Backend, corruptionTest bool) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))
	src := filepath.Join(tempDir(t), "source data")
	makeSource(t, src, rng)
	want1 := manifest(t, src)

	r, err := repo.Init(ctx, openBE())
	if err != nil {
		t.Fatal(err)
	}

	// --- full backup
	sn1, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sn1.Stats.Errors) > 0 {
		t.Fatalf("backup errors: %v", sn1.Stats.Errors)
	}
	t.Logf("sn1: %+v", sn1.Stats)
	if sn1.Stats.FilesNew != sn1.Stats.Files {
		t.Errorf("first backup: expected all files new, got %d/%d", sn1.Stats.FilesNew, sn1.Stats.Files)
	}
	// Compressible file must have been compressed.
	if sn1.Stats.BytesStored >= sn1.Stats.BytesAdded {
		t.Errorf("expected compression: added %d stored %d", sn1.Stats.BytesAdded, sn1.Stats.BytesStored)
	}

	// --- restore and compare
	tgt1 := filepath.Join(tempDir(t), "restore1")
	st, err := restorer.Run(ctx, r, sn1, restorer.Options{Target: tgt1, Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) > 0 {
		t.Fatalf("restore errors: %v", st.Errors)
	}
	if st.FilesVerified != st.Files {
		t.Errorf("verified %d of %d files", st.FilesVerified, st.Files)
	}
	compareManifests(t, want1, manifest(t, restoredRoot(t, tgt1, src)))

	// --- modify the source
	big := filepath.Join(src, "media", "random-30m.bin")
	f, err := os.OpenFile(big, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("CHANGED"), 15*mib); err != nil {
		t.Fatal(err)
	}
	f.Close()
	writeFile(t, filepath.Join(src, "new-file.txt"), []byte("added later"))
	if err := os.Remove(filepath.Join(src, "one-byte.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(src, "many", "d03")); err != nil {
		t.Fatal(err)
	}
	want2 := manifest(t, src)

	// --- incremental backup in a fresh session (re-open repository)
	r.Close()
	r, err = repo.Open(ctx, openBE())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sn2, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sn2: %+v", sn2.Stats)
	if sn2.Parent == nil || *sn2.Parent != sn1.ID {
		t.Errorf("sn2 parent: want %s", sn1.ID.Short())
	}
	if sn2.Stats.FilesChanged != 1 || sn2.Stats.FilesNew != 1 {
		t.Errorf("incremental: want 1 changed + 1 new, got %d changed %d new", sn2.Stats.FilesChanged, sn2.Stats.FilesNew)
	}
	if sn2.Stats.BytesRead != 30*mib+uint64(len("added later")) {
		t.Errorf("incremental should read only the changed file, read %d", sn2.Stats.BytesRead)
	}
	if sn2.Stats.BytesAdded > 9*mib {
		t.Errorf("dedup: changing 7 bytes added %d bytes", sn2.Stats.BytesAdded)
	}

	// --- both snapshots restore exactly
	tgt2 := filepath.Join(tempDir(t), "restore2")
	if st, err := restorer.Run(ctx, r, sn2, restorer.Options{Target: tgt2, Verify: true}); err != nil || len(st.Errors) > 0 {
		t.Fatalf("restore sn2: %v %v", err, st)
	}
	compareManifests(t, want2, manifest(t, restoredRoot(t, tgt2, src)))

	tgt3 := filepath.Join(tempDir(t), "restore3")
	if st, err := restorer.Run(ctx, r, sn1, restorer.Options{Target: tgt3}); err != nil || len(st.Errors) > 0 {
		t.Fatalf("restore sn1 again: %v %v", err, st)
	}
	compareManifests(t, want1, manifest(t, restoredRoot(t, tgt3, src)))

	// --- partial restore of one folder
	tgt4 := filepath.Join(tempDir(t), "restore4")
	st, err = restorer.Run(ctx, r, sn2, restorer.Options{Target: tgt4, Include: []string{filepath.Join(src, "docs")}})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("partial restore: %v %v", err, st)
	}
	if st.Files != 2 {
		t.Errorf("partial restore: want 2 files, got %d", st.Files)
	}
	gotDocs := manifest(t, filepath.Join(restoredRoot(t, tgt4, src), "docs"))
	wantDocs := map[string]entry{}
	for p, e := range want2 {
		if p == "docs" || strings.HasPrefix(p, "docs/") {
			wantDocs[strings.TrimPrefix(strings.TrimPrefix(p, "docs"), "/")] = e
		}
	}
	if e, ok := wantDocs[""]; ok {
		delete(wantDocs, "")
		wantDocs["."] = e
	}
	compareManifests(t, wantDocs, gotDocs)

	// --- full check
	res, err := checker.Run(ctx, r, checker.Options{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || len(res.Warnings) > 0 {
		t.Fatalf("check: errors %v warnings %v", res.Errors, res.Warnings)
	}
	if res.Snapshots != 2 || res.PacksRead != res.Packs {
		t.Errorf("check counts: %+v", res)
	}

	if corruptionTest {
		testCorruption(t, r)
	}
}

// testCorruption flips one byte in a pack and expects check and restore to notice.
func testCorruption(t *testing.T, r *repo.Repository) {
	ctx := context.Background()
	packs, err := r.ListPacks(ctx)
	if err != nil || len(packs) == 0 {
		t.Fatalf("list packs: %v", err)
	}
	loc := r.Backend().Location()
	// Pick the largest pack: it holds file data.
	var victim string
	var size int64
	for _, p := range packs {
		s := p.String()
		path := filepath.Join(loc, "data", s[:2], s)
		if fi, err := os.Stat(path); err == nil && fi.Size() > size {
			victim, size = path, fi.Size()
		}
	}
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	b[100] ^= 0xff
	if err := os.WriteFile(victim, b, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := checker.Run(ctx, r, checker.Options{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("check did not detect corruption")
	}
	t.Logf("corruption detected: %v", res.Errors)

	sn, err := r.LoadSnapshot(ctx, "latest")
	if err != nil {
		t.Fatal(err)
	}
	st, err := restorer.Run(ctx, r, sn, restorer.Options{Target: filepath.Join(tempDir(t), "corrupt")})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 {
		t.Fatal("restore from corrupted pack reported no errors")
	}
}

func TestBackupRestoreS3(t *testing.T) {
	srv, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx := context.Background()
	opts := backend.Options{S3AccessKey: "test", S3SecretKey: "test-secret", S3Region: "us-east-1"}
	loc := "s3://" + srv.Host + "/backups/office/pc1?tls=false"
	if _, err := backend.Open(ctx, "s3://"+srv.Host+"/missing?tls=false", opts); err == nil {
		t.Error("missing bucket accepted")
	}
	runCycle(t, func() backend.Backend {
		be, err := backend.Open(ctx, loc, opts)
		if err != nil {
			t.Fatal(err)
		}
		return be
	}, false)
}

// TestBackupRestoreSMB runs against a real SMB share, e.g.
//
//	BACKUPZIT_TEST_SMB=smb://user@host/share BACKUPZIT_TEST_SMB_PASSWORD=... go test ./internal/e2e -run SMB
func TestBackupRestoreSMB(t *testing.T) {
	loc := os.Getenv("BACKUPZIT_TEST_SMB")
	if loc == "" {
		t.Skip("BACKUPZIT_TEST_SMB not set")
	}
	ctx := context.Background()
	opts := backend.Options{SMBPassword: os.Getenv("BACKUPZIT_TEST_SMB_PASSWORD")}
	loc = fmt.Sprintf("%s/e2e-%d", strings.TrimRight(loc, "/"), time.Now().UnixNano())
	if _, err := backend.Open(ctx, loc, backend.Options{SMBPassword: "wrong"}); err == nil {
		t.Error("wrong SMB password accepted")
	}
	runCycle(t, func() backend.Backend {
		be, err := backend.Open(ctx, loc, opts)
		if err != nil {
			t.Fatal(err)
		}
		return be
	}, false)
}
