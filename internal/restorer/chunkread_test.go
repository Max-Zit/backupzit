package restorer_test

import (
	"bytes"
	"context"
	"errors"
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

// readShare answers pack reads after a moment, several at once like a
// share; it counts the reads running and can fail the n-th one.
type readShare struct {
	backend.Backend
	failAt           int32
	reads, now, most atomic.Int32
}

func (s *readShare) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if !strings.HasPrefix(name, "data/") {
		return s.Backend.LoadRange(ctx, name, off, n)
	}
	cur := s.now.Add(1)
	defer s.now.Add(-1)
	for m := s.most.Load(); cur > m && !s.most.CompareAndSwap(m, cur); m = s.most.Load() {
	}
	time.Sleep(15 * time.Millisecond)
	if s.reads.Add(1) == s.failAt {
		return nil, errors.New("connection reset")
	}
	return s.Backend.LoadRange(ctx, name, off, n)
}

// A file of many chunks: its chunks are read a few at a time and land in
// order; a failed read fails that file, and no read outlives the restore.
func TestRestoreReadsChunksAhead(t *testing.T) {
	ctx := context.Background()
	local, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Init(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	var content []repo.ID
	for i := range 15 {
		chunk := bytes.Repeat([]byte(fmt.Sprintf("chunk %02d.", i)), 3000)
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
	for _, failAt := range []int32{0, 7} {
		share := &readShare{Backend: local, failAt: failAt}
		r, err := repo.Open(ctx, share)
		if err != nil {
			t.Fatal(err)
		}
		target := t.TempDir()
		st, err := restorer.Run(ctx, r, &repo.Snapshot{Tree: root}, restorer.Options{Target: target})
		if n := share.now.Load(); n != 0 {
			t.Errorf("%d reads still running after the restore", n)
		}
		if failAt > 0 {
			if err == nil && len(st.Errors) == 0 {
				t.Error("a failed chunk read was not reported")
			}
		} else {
			if err != nil || len(st.Errors) > 0 {
				t.Fatal(err, st.Errors)
			}
			got, err := os.ReadFile(filepath.Join(target, "f.bin"))
			if err != nil || !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("restored file differs (%v)", err)
			}
			if m := share.most.Load(); m < 2 {
				t.Errorf("%d chunk reads at once, want several", m)
			}
		}
		_ = r.Close()
	}
}
