package checker_test

import (
	"context"
	"fmt"
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

// LoadMax goes through Load: the data check reads packs with a size limit
// (MED 5, 0.63), and the LoadMax of Local would bypass the fault.
func (c *cutShort) LoadMax(ctx context.Context, name string, _ int64) ([]byte, error) {
	return c.Load(ctx, name)
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

// down fails every read of a data pack, like a storage that stopped
// answering in the middle of a check.
type down struct{ *backend.Local }

func (d *down) Load(ctx context.Context, name string) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		return nil, os.ErrDeadlineExceeded
	}
	return d.Local.Load(ctx, name)
}

func (d *down) LoadMax(ctx context.Context, name string, _ int64) ([]byte, error) {
	return d.Load(ctx, name)
}

func (d *down) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		return nil, os.ErrDeadlineExceeded
	}
	return d.Local.LoadRange(ctx, name, off, n)
}

// A4 (0.55): when the storage stops answering, the check stops after a
// streak of unreadable packs instead of trying each one (with its pauses)
// for days, and judges nothing.
func TestCheckStopsWhenStorageIsDown(t *testing.T) {
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
	for i := 0; i < 30; i++ {
		data := make([]byte, 64<<10)
		mrand.New(mrand.NewSource(int64(i))).Read(data)
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if err := r.Flush(ctx); err != nil { // one pack each
			t.Fatal(err)
		}
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
	r.Close()
	r2, err := repo.Open(ctx, &down{Local: loc})
	if err != nil {
		t.Fatal(err)
	}
	for _, readData := range []bool{false, true} {
		start := time.Now()
		res, err := checker.Run(ctx, r2, checker.Options{ReadData: readData, RetryPause: time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "stopped answering") {
			t.Fatalf("ReadData=%v: err %v, want the storage-down stop", readData, err)
		}
		if res != nil && (len(res.Damaged) > 0 || len(res.Unreadable) > 0) {
			t.Errorf("ReadData=%v: judged packs while the storage was down: %+v", readData, res)
		}
		if time.Since(start) > time.Minute {
			t.Errorf("ReadData=%v: took %v", readData, time.Since(start))
		}
	}
}

// shortList leaves packs out of the data listing (an SFTP/SMB listing that
// skips a folder it cannot read) while they are still on the storage.
type shortList struct {
	*backend.Local
	hide  map[string]bool
	empty bool
}

func (s *shortList) List(ctx context.Context, dir string) ([]string, error) {
	names, err := s.Local.List(ctx, dir)
	if err != nil || dir != "data" {
		return names, err
	}
	if s.empty {
		return nil, nil
	}
	var out []string
	for _, n := range names {
		if !s.hide[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// H-N6 (0.56): a pack missing from one listing is not damaged while the
// storage still has it; an empty data listing is no verdict at all; a pack
// that is really gone still is damaged.
func TestPackMissingFromListing(t *testing.T) {
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
		data := make([]byte, 64<<10)
		mrand.New(mrand.NewSource(int64(100 + i))).Read(data)
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
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
	packs, _ := r.ListPacks(ctx)
	r.Close()
	name := func(p repo.ID) string { return "data/" + p.String()[:2] + "/" + p.String() }

	// Two packs left out of the listing, still stored: warnings, no damage.
	sl := &shortList{Local: loc, hide: map[string]bool{name(packs[0]): true, name(packs[1]): true}}
	r2, err := repo.Open(ctx, sl)
	if err != nil {
		t.Fatal(err)
	}
	res, err := checker.Run(ctx, r2, checker.Options{ReadData: true, RetryPause: time.Millisecond})
	if err != nil || len(res.Damaged) != 0 || len(res.Warnings) < 2 || !res.OK() {
		t.Fatalf("incomplete listing: err %v, damaged %v, warnings %v, errors %v", err, res.Damaged, res.Warnings, res.Errors)
	}
	// An empty data listing: an error, nothing judged.
	r3, _ := repo.Open(ctx, &shortList{Local: loc, empty: true})
	if res, err := checker.Run(ctx, r3, checker.Options{RetryPause: time.Millisecond}); err == nil || (res != nil && len(res.Damaged) > 0) {
		t.Fatalf("empty listing: err %v, %+v", err, res)
	}
	// One pack really gone: damaged.
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(name(packs[2])))); err != nil {
		t.Fatal(err)
	}
	r4, _ := repo.Open(ctx, loc)
	res, err = checker.Run(ctx, r4, checker.Options{RetryPause: time.Millisecond})
	if err != nil || len(res.Damaged) != 1 || res.Damaged[0] != packs[2] {
		t.Fatalf("missing pack: err %v, damaged %v", err, res.Damaged)
	}
}

type vanished struct{ *backend.Local }

func (v *vanished) Load(ctx context.Context, name string) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		return nil, fmt.Errorf("%s: %w", name, backend.ErrNotFound)
	}
	return v.Local.Load(ctx, name)
}

func (v *vanished) LoadMax(ctx context.Context, name string, _ int64) ([]byte, error) {
	return v.Load(ctx, name)
}

// B4 (0.59): packs the storage just listed and that are gone when the data
// check reads them, most of them at once, are a folder that went away
// during the check (a data volume unmounted), not lost data: no verdict.
func TestCheckJudgesNothingWhenMostPacksVanish(t *testing.T) {
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
	for i := 0; i < 30; i++ {
		data := make([]byte, 64<<10)
		mrand.New(mrand.NewSource(int64(i))).Read(data)
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
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
	r.Close()
	r2, err := repo.Open(ctx, &vanished{Local: loc})
	if err != nil {
		t.Fatal(err)
	}
	res, err := checker.Run(ctx, r2, checker.Options{ReadData: true, RetryPause: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "gone when they were read") {
		t.Fatalf("err %v, want no verdict", err)
	}
	if res != nil && len(res.Damaged) > 0 {
		t.Errorf("judged %d packs damaged", len(res.Damaged))
	}
}
