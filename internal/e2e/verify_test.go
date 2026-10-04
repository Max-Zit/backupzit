package e2e

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

func TestSampleVerify(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(tempDir(t), "repo")
	be, _ := backend.OpenLocal(dir)
	r, err := repo.Init(ctx, be, repo.Password("x"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tempDir(t), "src")
	makeSource(t, src, rand.New(rand.NewSource(9)))
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := restorer.SampleVerify(ctx, r, sn, restorer.SampleOptions{Files: 20, MaxBytes: 20 << 20, TempDir: tempDir(t)})
	if err != nil || len(st.Errors) > 0 || st.Files == 0 || st.Files > 20 || st.Bytes > 20<<20+30<<20 {
		t.Fatalf("sample verify: %+v %v", st, err)
	}
	t.Logf("verified %d of %d files, %d bytes", st.Files, st.Candidates, st.Bytes)

	// Damage every pack: the test must notice.
	packs, _ := filepath.Glob(filepath.Join(dir, "data", "*", "*"))
	for _, p := range packs {
		b, _ := os.ReadFile(p)
		for i := 100; i < len(b)-100; i += 4096 {
			b[i] ^= 0xff
		}
		os.WriteFile(p, b, 0o600)
	}
	r.Close()
	be, _ = backend.OpenLocal(dir)
	r, _ = repo.Open(ctx, be, repo.Password("x"))
	st, err = restorer.SampleVerify(ctx, r, sn, restorer.SampleOptions{Files: 20, MaxBytes: 20 << 20, TempDir: tempDir(t)})
	if err == nil && len(st.Errors) == 0 {
		t.Fatalf("corruption not detected: %+v", st)
	}
}
