package checker_test

import (
	"context"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
)

// cutShort delivers one file with only half of its bytes while the storage
// holds it whole, like a network path that truncates downloads (#38).
type cutShort struct {
	*backend.Local
	name string
}

func (c *cutShort) Load(ctx context.Context, name string) ([]byte, error) {
	b, err := c.Local.Load(ctx, name)
	if err == nil && name == c.name {
		b = b[:len(b)/2]
	}
	return b, err
}

// R-1 (0.55): a pack that only arrives short counts as unreadable from this
// agent, never as damaged, so repair leaves it alone; a pack that is really
// shorter on the storage is damaged.
func TestShortDownloadIsNotDamage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	loc, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	var ids []repo.ID
	for i := 0; i < 4; i++ {
		data := make([]byte, 6<<20)
		mrand.New(mrand.NewSource(int64(i))).Read(data)
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	tree, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "f", Type: repo.NodeFile, Content: ids}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveSnapshot(ctx, &repo.Snapshot{Time: time.Now(), Tree: tree}); err != nil {
		t.Fatal(err)
	}
	packs, err := r.ListPacks(ctx)
	if err != nil || len(packs) < 2 {
		t.Fatalf("packs: %v %v", packs, err)
	}
	r.Close()
	name := "data/" + packs[0].String()[:2] + "/" + packs[0].String()

	// Cut short on the way: unreadable, not damaged.
	r2, err := repo.Open(ctx, &cutShort{Local: loc, name: name})
	if err != nil {
		t.Fatal(err)
	}
	res, err := checker.Run(ctx, r2, checker.Options{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Damaged) != 0 || len(res.Unreadable) != 1 || res.Unreadable[0] != packs[0] {
		t.Fatalf("damaged %v, unreadable %v, errors %v", res.Damaged, res.Unreadable, res.Errors)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "network path") {
		t.Errorf("errors: %v", res.Errors)
	}
	r2.Close()

	// Really shorter on the storage: damaged.
	p := filepath.Join(dir, filepath.FromSlash(name))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(p, 0o600)
	if err := os.WriteFile(p, b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	r3, err := repo.Open(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	res, err = checker.Run(ctx, r3, checker.Options{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Damaged) != 1 || res.Damaged[0] != packs[0] || len(res.Unreadable) != 0 {
		t.Fatalf("pack short on the storage: damaged %v, unreadable %v, errors %v", res.Damaged, res.Unreadable, res.Errors)
	}
}
