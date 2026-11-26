package repo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// rangeLog records the pack reads of a source repository.
type rangeLog struct {
	backend.Backend
	mu      sync.Mutex
	reads   []string // "pack@offset"
	failAt  int32
	n       atomic.Int32
	running atomic.Int32
}

func (l *rangeLog) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		l.running.Add(1)
		defer l.running.Add(-1)
		l.mu.Lock()
		l.reads = append(l.reads, fmt.Sprintf("%s@%010d", name, off))
		l.mu.Unlock()
		if l.n.Add(1) == l.failAt {
			time.Sleep(20 * time.Millisecond)
			return nil, errors.New("connection reset")
		}
	}
	return l.Backend.LoadRange(ctx, name, off, n)
}

// copySource makes a repository with three backups of one file of many
// chunks each: the second shares most of its data with the first.
func copySource(t *testing.T) (*repo.Repository, *rangeLog, []*repo.Snapshot, [][]byte) {
	t.Helper()
	ctx := context.Background()
	local, err := backend.OpenLocal(filepath.Join(t.TempDir(), "src"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Init(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, 8))
	chunk := func() []byte {
		b := make([]byte, 64<<10)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		return b
	}
	shared := make([][]byte, 40)
	for i := range shared {
		shared[i] = chunk()
	}
	var files [][]byte
	for k := range 3 {
		chunks := shared
		if k == 2 {
			chunks = nil
			for range 30 {
				chunks = append(chunks, chunk())
			}
		} else if k == 1 {
			chunks = append(append([][]byte{}, shared...), chunk(), chunk())
		}
		var content []repo.ID
		var whole []byte
		for _, c := range chunks {
			id, _, err := w.SaveBlob(ctx, repo.DataBlob, c)
			if err != nil {
				t.Fatal(err)
			}
			content = append(content, id)
			whole = append(whole, c...)
		}
		tree, err := w.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "f.bin", Type: repo.NodeFile, Mode: 0o644, Size: uint64(len(whole)), Content: content}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := w.SaveSnapshot(ctx, &repo.Snapshot{Time: time.Date(2026, 11, 20+k, 3, 0, 0, 0, time.UTC), Hostname: "h", Paths: []string{"/f"}, Tree: tree, Tags: []string{"job:1"}}); err != nil {
			t.Fatal(err)
		}
		files = append(files, whole)
	}
	_ = w.Close()
	log := &rangeLog{Backend: local}
	src, err := repo.Open(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	sns, err := src.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sns) != 3 {
		t.Fatalf("%d snapshots", len(sns))
	}
	return src, log, sns, files
}

func readFile(t *testing.T, r *repo.Repository, sn *repo.Snapshot) []byte {
	t.Helper()
	ctx := context.Background()
	tr, err := r.LoadTree(ctx, sn.Tree)
	if err != nil {
		t.Fatal(err)
	}
	var b []byte
	for _, id := range tr.Nodes[0].Content {
		d, err := r.LoadBlob(ctx, repo.DataBlob, id)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, d...)
	}
	return b
}

// A copy first adds up what the destination lacks, then copies it in pack
// order with few requests and reports how far it is; the copies read back
// right; a second copy skips everything (#75).
func TestCopyPlansAndReportsProgress(t *testing.T) {
	ctx := context.Background()
	src, log, sns, files := copySource(t)
	dlocal, err := backend.OpenLocal(filepath.Join(t.TempDir(), "dst"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := repo.Init(ctx, dlocal)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var reports []repo.CopyProgress
	opts := &repo.CopyOptions{JobTag: "job:9", Progress: func(p repo.CopyProgress) { reports = append(reports, p) }}
	st, copied, err := repo.Copy(ctx, src, dst, sns, []string{"job:9"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshots != 3 || !st.First || st.Packed == 0 || st.Seconds <= 0 || st.BytesStored == 0 {
		t.Errorf("stats %+v", st)
	}
	var planned, copiedPhase bool
	var total uint64
	for i, p := range reports {
		if p.Phase == repo.CopyPlanning {
			planned = true
			if copiedPhase {
				t.Error("planning after copying")
			}
		}
		if p.Phase == repo.CopyTransfer {
			if !copiedPhase {
				total = p.Total
			}
			copiedPhase = true
			if p.Total != total || p.Done > p.Total {
				t.Errorf("report %d: %d of %d (total was %d)", i, p.Done, p.Total, total)
			}
		}
		if p.Snapshots != 3 || p.Snapshot < 1 || p.Snapshot > 3 || !p.First || p.Time.IsZero() {
			t.Errorf("report %d: %+v", i, p)
		}
	}
	if last := reports[len(reports)-1]; !planned || !copiedPhase || last.Done != last.Total || last.Done != st.Packed {
		t.Errorf("last report %+v, packed %d", last, st.Packed)
	}
	// 112 data blobs and 3 trees went in far fewer requests, in pack order.
	log.mu.Lock()
	reads := append([]string(nil), log.reads...)
	log.mu.Unlock()
	if len(reads) == 0 || len(reads) > 20 {
		t.Errorf("%d pack reads for %d blobs", len(reads), st.Blobs)
	}
	for i, sn := range copied {
		if got := readFile(t, dst, sn); !bytes.Equal(got, files[i]) {
			t.Errorf("copy %d reads back wrong", i)
		}
	}
	reports = nil
	st, _, err = repo.Copy(ctx, src, dst, sns, []string{"job:9"}, opts)
	if err != nil || st.Snapshots != 0 || st.Skipped != 3 || st.First || st.Packed != 0 {
		t.Errorf("second copy: %+v %v", st, err)
	}
}

// A failed read ends the copy with that error; the backups copied before
// stay, and no read is left running.
func TestCopyReadErrorKeepsEarlierCopies(t *testing.T) {
	ctx := context.Background()
	src, log, sns, files := copySource(t)
	log.mu.Lock()
	log.reads = nil
	log.mu.Unlock()
	dlocal, err := backend.OpenLocal(filepath.Join(t.TempDir(), "dst"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := repo.Init(ctx, dlocal)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	// The plan reads the trees (3 reads); the copy of the first backup
	// reads a few ranges; fail a read of the third backup.
	var firstDone int
	opts := &repo.CopyOptions{Progress: func(p repo.CopyProgress) {
		if p.Phase == repo.CopyTransfer && p.Snapshot == 3 && firstDone == 0 {
			firstDone = int(log.n.Load())
			log.failAt = int32(firstDone) + 1
		}
	}}
	st, copied, err := repo.Copy(ctx, src, dst, sns, nil, opts)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("error %v", err)
	}
	if n := log.running.Load(); n != 0 {
		t.Errorf("%d reads still running", n)
	}
	if st.Snapshots != 2 || len(copied) != 2 {
		t.Fatalf("%d backups copied before the error, want 2", st.Snapshots)
	}
	for i, sn := range copied {
		if got := readFile(t, dst, sn); !bytes.Equal(got, files[i]) {
			t.Errorf("copy %d reads back wrong", i)
		}
	}
}
