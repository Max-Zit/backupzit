package restorer_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// slowShare answers ranged pack reads after a delay, several at once, like
// an SMB share; it counts the most reads in flight.
type slowShare struct {
	backend.Backend
	now, maxSeen atomic.Int32
}

func (s *slowShare) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		cur := s.now.Add(1)
		defer s.now.Add(-1)
		for {
			m := s.maxSeen.Load()
			if cur <= m || s.maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return s.Backend.LoadRange(ctx, name, off, n)
}

// The chunks of a file are read several at a time from a share and still
// land in order; a restore test reads its sample blocks the same way.
func TestRestoreReadsChunksAhead(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	local, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Init(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	var content []repo.ID
	for i := range 12 {
		chunk := bytes.Repeat([]byte(fmt.Sprintf("chunk %02d|", i)), 1000)
		id, _, err := w.SaveBlob(ctx, repo.DataBlob, chunk)
		if err != nil {
			t.Fatal(err)
		}
		content = append(content, id)
		want.Write(chunk)
	}
	root, err := w.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "f.bin", Type: repo.NodeFile, Mode: 0o644,
		Size: uint64(want.Len()), Content: content}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	w.Close()

	share := &slowShare{Backend: local}
	r, err := repo.Open(ctx, share)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	target := t.TempDir()
	st, err := restorer.Run(ctx, r, &repo.Snapshot{Tree: root}, restorer.Options{Target: target})
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st.Errors)
	}
	got, err := os.ReadFile(filepath.Join(target, "f.bin"))
	if err != nil || !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("restored file differs (%v)", err)
	}
	if m := share.maxSeen.Load(); m < 2 {
		t.Errorf("%d chunk reads at once, want several", m)
	}
}
