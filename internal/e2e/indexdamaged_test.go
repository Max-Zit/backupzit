package e2e

import (
	"bytes"
	"context"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/testutil"
)

// Review of #53 (1): RebuildIndex moves a damaged index file to
// index-damaged/. Every backend must accept that folder, must not list it
// with index/, and the check and prune must work afterwards.
func TestDamagedIndexMovedAsideOnEveryBackend(t *testing.T) {
	ctx := context.Background()
	hsrv, hopts := startHardened(t)
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s3.Close)
	sftp, err := testutil.StartSFTPServer("backup", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sftp.Close() })
	localDir := filepath.Join(tempDir(t), "local")
	for name, open := range map[string]func() backend.Backend{
		"local": func() backend.Backend {
			be, err := backend.OpenLocal(localDir)
			if err != nil {
				t.Fatal(err)
			}
			return be
		},
		"hardened": func() backend.Backend { return openOrFail(t, "hardened://"+hsrv.Host+"/office/pc1", hopts) },
		"s3": func() backend.Backend {
			return openOrFail(t, "s3://"+s3.Host+"/backups/office/pc1?tls=false", backend.Options{S3AccessKey: "test", S3SecretKey: "test-secret", S3Region: "us-east-1"})
		},
		"sftp": func() backend.Backend {
			return openOrFail(t, sftp.URL("repo"), backend.Options{SFTPPassword: "s3cret", SFTPHostKey: sftp.Fingerprint})
		},
	} {
		t.Run(name, func(t *testing.T) {
			be := open()
			r, err := repo.Init(ctx, be, repo.Password("pw"))
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewSource(7))
			var ids []repo.ID
			for range 6 {
				b := make([]byte, 20<<10)
				rng.Read(b)
				id, _, err := r.SaveBlob(ctx, repo.DataBlob, b)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			if err := r.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			r.Close()

			// An index file that cannot be read (a new name: hardened
			// repositories keep what they have stored).
			bad := "index/" + strings.Repeat("9f", 32)
			garbage := []byte("not an index")
			if err := open().Save(ctx, bad, garbage); err != nil {
				t.Fatal(err)
			}
			be2 := open()
			r2, err := repo.Open(ctx, be2, repo.Password("pw"))
			if err != nil {
				t.Fatal(err)
			}
			if got := r2.DamagedIndexFiles(); len(got) != 1 || got[0] != bad {
				t.Fatalf("damaged: %v", got)
			}
			rb, err := r2.RebuildIndex(ctx)
			if err != nil || len(rb.Moved) != 1 {
				t.Fatal(rb, err)
			}
			r2.Close()

			be3 := open()
			defer be3.Close()
			aside := "index-damaged/" + strings.Repeat("9f", 32)
			// The copy is sealed with the repository's key (0.59).
			if got, err := be3.Load(ctx, aside); err != nil || bytes.Contains(got, garbage) {
				t.Fatalf("%s: %q %v", aside, got, err)
			}
			if names, err := be3.List(ctx, "index"); err != nil || slices.Contains(names, bad) || slices.Contains(names, aside) {
				t.Errorf("index/ lists %v (%v)", names, err)
			}
			r3, err := repo.Open(ctx, be3, repo.Password("pw"))
			if err != nil {
				t.Fatal(err)
			}
			if got := r3.DamagedIndexFiles(); len(got) != 0 {
				t.Fatalf("still damaged: %v", got)
			}
			if got, err := r3.DamagedIndexCopy(ctx, bad); err != nil || !bytes.Equal(got, garbage) {
				t.Fatalf("copy of %s: %q %v", bad, got, err)
			}
			for _, id := range ids {
				if _, err := r3.LoadBlob(ctx, repo.DataBlob, id); err != nil {
					t.Fatal(err)
				}
			}
			if res, err := checker.Run(ctx, r3, checker.Options{ReadData: true}); err != nil || !res.OK() {
				t.Fatalf("check: %v %v", res, err)
			}
			if _, err := r3.Prune(ctx, repo.PruneOptions{}); err != nil {
				t.Fatalf("prune: %v", err)
			}
			if _, err := be3.Size(ctx, aside); err != nil {
				t.Errorf("prune removed %s: %v", aside, err)
			}
		})
	}
}

func openOrFail(t *testing.T, loc string, opts backend.Options) backend.Backend {
	t.Helper()
	be, err := backend.Open(context.Background(), loc, opts)
	if err != nil {
		t.Fatal(err)
	}
	return be
}
