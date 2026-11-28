package checker_test

import (
	"context"
	"fmt"
	mrand "math/rand"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
)

// tooLarge answers like a storage holding one file larger than any pack:
// the size limit of LoadMax refuses it.
type tooLarge struct {
	*backend.Local
	name  string
	reads int
}

func (b *tooLarge) LoadMax(ctx context.Context, name string, max int64) ([]byte, error) {
	if name == b.name {
		b.reads++
		return nil, fmt.Errorf("%s is larger than %d bytes: %w", name, max, backend.ErrTooLarge)
	}
	return b.Local.LoadMax(ctx, name, max)
}

// MED 5 (0.63): an indexed pack over the size limit is reported on its own,
// read once, and never counted as damaged (repair would take it out) or as
// unreadable from this agent.
func TestPackOverTheLimitIsNotDamaged(t *testing.T) {
	ctx := context.Background()
	loc, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	var ids []repo.ID
	for i := 0; i < 3; i++ {
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
	if err != nil || len(packs) < 1 {
		t.Fatalf("packs: %v %v", packs, err)
	}
	r.Close()
	big := packs[0]
	be := &tooLarge{Local: loc, name: "data/" + big.String()[:2] + "/" + big.String()}
	r2, err := repo.Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	res, err := checker.Run(ctx, r2, checker.Options{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Damaged) != 0 || len(res.Unreadable) != 0 || len(res.TooLarge) != 1 || res.TooLarge[0] != big {
		t.Fatalf("damaged %v, unreadable %v, too large %v, errors %v", res.Damaged, res.Unreadable, res.TooLarge, res.Errors)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "repair leaves it alone") || res.OK() {
		t.Errorf("errors: %v", res.Errors)
	}
	if be.reads != 1 {
		t.Errorf("%d reads of the pack over the limit, want 1", be.reads)
	}
	if res.PacksRead != len(packs)-1 {
		t.Errorf("read %d of the other %d packs", res.PacksRead, len(packs)-1)
	}
}
