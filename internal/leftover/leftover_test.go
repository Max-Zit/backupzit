package leftover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "leftovers.json")
	Open(path)
	defer Close()
	ctx := WithRun(context.Background(), 7)
	snap := Item{Platform: Proxmox, Kind: KindSnapshot, VM: "101", Name: "backupzit_1"}
	lock := Item{Platform: Proxmox, Kind: KindLock, VM: "102", Name: "backup"}
	Add(ctx, snap)
	Add(context.Background(), lock)
	if got := List(); len(got) != 2 || got[0].RunID != 7 || got[0].Created.IsZero() {
		t.Fatalf("list %+v", got)
	}
	// Report leaves out what a running run made, unless its removal failed.
	running := func(id int64) bool { return id == 7 }
	if got := Report(running); len(got) != 1 || got[0].Kind != KindLock {
		t.Fatalf("report while run 7 runs: %+v", got)
	}
	Failed(snap, errors.New("storage busy"))
	Failed(snap, nil)
	got := List()
	if len(got) != 2 || got[0].Tries != 2 || got[0].Error != "removal failed" || got[0].LastTry.IsZero() {
		t.Fatalf("after failures %+v", got)
	}
	if r := Report(running); len(r) != 2 {
		t.Fatalf("a failed removal is reported: %+v", r)
	}
	// Adding again keeps the first time and the tries.
	first := got[0].Created
	time.Sleep(10 * time.Millisecond)
	Add(ctx, snap)
	if got := List(); !got[0].Created.Equal(first) || got[0].Tries != 2 {
		t.Fatalf("re-added %+v", got[0])
	}
	Done(snap)
	Done(lock)
	if got := List(); len(got) != 0 {
		t.Fatalf("after done %+v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty ledger file left: %v", err)
	}
	// A damaged file does not stop the recording.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	Add(ctx, snap)
	if got := List(); len(got) != 1 {
		t.Fatalf("after a damaged file %+v", got)
	}
}

func TestLedgerBounded(t *testing.T) {
	Open(filepath.Join(t.TempDir(), "l.json"))
	defer Close()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < maxItems+20; i++ {
		Add(context.Background(), Item{Platform: VMware, Kind: KindSnapshot, VM: "vm", Name: time.Duration(i).String(), Created: base.Add(time.Duration(i) * time.Second)})
	}
	got := List()
	if len(got) != maxItems || got[0].Name != time.Duration(20).String() {
		t.Fatalf("%d items, first %q", len(got), got[0].Name)
	}
}

func TestWithoutLedger(t *testing.T) {
	Close()
	Add(context.Background(), Item{Platform: HyperV, Kind: KindCheckpoint, VM: "x", Name: "y"})
	Failed(Item{Platform: HyperV}, nil)
	if List() != nil {
		t.Fatal("items without a ledger")
	}
}

func TestRetry(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), []time.Duration{time.Millisecond, time.Millisecond},
		func(context.Context) error { calls++; return errors.New("busy") },
		func(context.Context) (bool, error) { return calls >= 2, nil })
	if err != nil || calls != 2 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
	calls = 0
	err = Retry(context.Background(), []time.Duration{time.Millisecond},
		func(context.Context) error { calls++; return nil },
		func(context.Context) (bool, error) { return false, nil })
	if err == nil || calls != 2 {
		t.Fatalf("still there: err %v after %d calls", err, calls)
	}
	// A removal that reports an error but did remove the object counts.
	if err := Retry(context.Background(), nil, func(context.Context) error { return errors.New("timeout") },
		func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
}

// An item whose VM is missing from the host's VM list is kept (not shown)
// for MissingGrace, so it is still removed when the VM comes back; then it
// is dropped.
func TestMissingVM(t *testing.T) {
	Open(filepath.Join(t.TempDir(), "leftovers.json"))
	defer Close()
	snap := Item{Platform: VMware, Kind: KindSnapshot, Host: "esx1", VM: "5021", Name: "backupzit_1"}
	Add(context.Background(), snap)
	Failed(snap, errors.New("task timed out"))
	none := func(int64) bool { return false }
	if Missing(snap) {
		t.Fatal("dropped at once")
	}
	got := List()
	if len(got) != 1 || got[0].MissingSince.IsZero() || got[0].Tries != 1 || got[0].Error == "" {
		t.Fatalf("after missing once: %+v", got)
	}
	if r := Report(none); len(r) != 0 {
		t.Fatalf("a missing VM's item is reported: %+v", r)
	}
	if Missing(got[0]) {
		t.Fatal("dropped within the grace period")
	}
	since := List()[0].MissingSince
	// Back in the list: shown again with its history.
	Found(got[0])
	if r := Report(none); len(r) != 1 || !r[0].MissingSince.IsZero() || r[0].Tries != 1 {
		t.Fatalf("after the VM came back: %+v", r)
	}
	if since.IsZero() {
		t.Fatal("no time recorded")
	}
	// Missing for longer than the grace period: dropped.
	old := MissingGrace
	MissingGrace = 0
	defer func() { MissingGrace = old }()
	Missing(snap)
	if !Missing(snap) || len(List()) != 0 {
		t.Fatalf("not dropped after the grace period: %+v", List())
	}
}
