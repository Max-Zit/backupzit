package checker_test

import (
	"context"
	mrand "math/rand"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
)

// slowPacks loads packs after a delay, several at once, like a share; it
// counts the most loads in flight.
type slowPacks struct {
	backend.Backend
	now, maxSeen atomic.Int32
}

func (s *slowPacks) Load(ctx context.Context, name string) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		cur := s.now.Add(1)
		defer s.now.Add(-1)
		for {
			m := s.maxSeen.Load()
			if cur <= m || s.maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	return s.Backend.Load(ctx, name)
}

// On a share (one upload at a time) the data check still reads several
// packs at once and reads every pack.
func TestReadDataOnShareReadsSeveralPacks(t *testing.T) {
	ctx := context.Background()
	local, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Init(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	var ids []repo.ID
	for i := 0; i < 8; i++ { // random 6 MiB blocks: several ~16 MiB packs
		data := make([]byte, 6<<20)
		mrand.New(mrand.NewSource(int64(i))).Read(data)
		id, _, err := w.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	tree, err := w.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "f", Type: repo.NodeFile, Content: ids}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.SaveSnapshot(ctx, &repo.Snapshot{Time: time.Now(), Tree: tree}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	share := &slowPacks{Backend: local}
	r, err := repo.Open(ctx, share)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	packs, err := r.ListPacks(ctx)
	if err != nil || len(packs) < 3 {
		t.Fatalf("packs: %v %v", packs, err)
	}
	res, err := checker.Run(ctx, r, checker.Options{ReadData: true})
	if err != nil || !res.OK() {
		t.Fatal(err, res.Errors)
	}
	if res.PacksRead != len(packs) {
		t.Errorf("read %d of %d packs", res.PacksRead, len(packs))
	}
	if m := share.maxSeen.Load(); m < 2 {
		t.Errorf("%d packs read at once, want several", m)
	}
}
