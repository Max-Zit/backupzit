package archiver

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/restic/chunker"
)

// failAfter reads its file but fails once n bytes were read.
type failAfter struct {
	io.ReadCloser
	n int
}

func (f *failAfter) Read(b []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("read error (test)")
	}
	if len(b) > f.n {
		b = b[:f.n]
	}
	k, err := f.ReadCloser.Read(b)
	f.n -= k
	return k, err
}

// failReads makes openSource fail reading after n bytes while *on is true.
func failReads(t *testing.T, n int, on *bool) {
	t.Helper()
	old := openSource
	t.Cleanup(func() { openSource = old })
	openSource = func(p string) (io.ReadCloser, error) {
		f, err := old(p)
		if err != nil || !*on {
			return f, err
		}
		return &failAfter{ReadCloser: f, n: n}, nil
	}
}

func testRepo(t *testing.T) *repo.Repository {
	t.Helper()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// MED 3 (0.63): when the read of a hard-linked file failed part-way, its
// chunks so far were kept for the file's other names: the next name was
// stored with the truncated content and counted as unchanged. It is now read
// itself; a name after a complete read still shares the content.
func TestHardLinkNotSharedAfterFailedRead(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	bw, err := repo.NewBlobWriter(ctx, r, 1) // chunks saved as they come
	if err != nil {
		t.Fatal(err)
	}
	defer bw.Close()
	a := &Archiver{r: r, bw: bw, buf: make([]byte, chunker.MaxSize), chk: chunker.New(nil, r.ChunkerPolynomial())}
	data := make([]byte, 24<<20)
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	failing := true
	// Chunks are at most 8 MiB, so at least two are cut and one saved
	// before the read fails at 20 MiB, whatever the chunker polynomial.
	failReads(t, 20<<20, &failing)
	link := func(name string) repo.Node {
		return repo.Node{Name: name, Type: repo.NodeFile, Unix: &repo.UnixMeta{LinkKey: "1:2"}}
	}

	first := link("a")
	if err := a.archiveFile(ctx, p, fi, &first, nil); err == nil {
		t.Fatal("the read error was not reported")
	}
	if len(first.Content) == 0 {
		t.Fatal("no chunk was stored before the read error; the test needs some")
	}
	failing = false
	second := link("b")
	if err := a.archiveFile(ctx, p, fi, &second, nil); err != nil {
		t.Fatal(err)
	}
	if a.stats.FilesSkipped != 0 || len(second.Content) <= len(first.Content) {
		t.Fatalf("the second name took the content of the failed read: %d chunks (failed read: %d), %d skipped",
			len(second.Content), len(first.Content), a.stats.FilesSkipped)
	}
	third := link("c")
	if err := a.archiveFile(ctx, p, fi, &third, nil); err != nil {
		t.Fatal(err)
	}
	if a.stats.FilesSkipped != 1 || len(third.Content) != len(second.Content) {
		t.Errorf("a name after a complete read is not shared: %d chunks, %d skipped", len(third.Content), a.stats.FilesSkipped)
	}
}

// L-N22 (0.63): with CompleteOnly a backup that could not read a file is
// returned with its errors but not saved: a SQL backup missing a dump is no
// restore point, and a saved snapshot with the job's tag counted for the
// job's retention.
func TestCompleteOnlyDoesNotSaveIncompleteSnapshot(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	src := t.TempDir()
	for _, n := range []string{"db1.dump", "db2.dump"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte("dump of "+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	failing := true
	failReads(t, 0, &failing)
	opts := Options{Paths: []string{src}, NoParent: true, Hostname: "h", Tags: []string{"job:7"}, CompleteOnly: true}
	sn, err := Run(ctx, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(sn.Stats.Errors) == 0 {
		t.Fatal("no read error recorded")
	}
	if !sn.ID.IsNull() {
		t.Errorf("the incomplete snapshot has an ID: %s", sn.ID)
	}
	if sns, err := r.ListSnapshots(ctx); err != nil || len(sns) != 0 {
		t.Fatalf("snapshots in the repository: %d %v", len(sns), err)
	}
	// Complete: saved as before.
	failing = false
	sn, err = Run(ctx, r, opts)
	if err != nil || len(sn.Stats.Errors) > 0 {
		t.Fatal(err, sn.Stats.Errors)
	}
	if sns, err := r.ListSnapshots(ctx); err != nil || len(sns) != 1 || sns[0].ID != sn.ID {
		t.Fatalf("the complete snapshot was not saved: %d %v", len(sns), err)
	}
}
