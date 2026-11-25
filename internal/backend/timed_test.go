package backend

import (
	"context"
	"strings"
	"testing"
	"time"
)

type concurrentLocal struct{ Backend }

func (concurrentLocal) Concurrency() int { return 4 }

// Timed counts calls and bytes and keeps the upload concurrency of the
// backend it wraps.
func TestTimed(t *testing.T) {
	local, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tb := NewTimed(local)
	if err := tb.Save(ctx, "data/ab/x", []byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.LoadRange(ctx, "data/ab/x", 6, 5); err != nil {
		t.Fatal(err)
	}
	rep := tb.Report(time.Second)
	for _, want := range []string{"save 1 calls", "loadrange 1 calls"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report %q lacks %q", rep, want)
		}
	}
	if strings.Contains(rep, "load 0") || strings.Contains(rep, "; load ") {
		t.Errorf("report %q lists loads that never happened", rep)
	}
	if n := Concurrency(NewTimed(local)); n != 1 {
		t.Errorf("concurrency of a timed local disk %d", n)
	}
	if n := Concurrency(NewTimed(concurrentLocal{local})); n != 4 {
		t.Errorf("concurrency of a timed object store %d", n)
	}
	if n := ReadConcurrency(NewTimed(local)); n != shareReadConcurrency {
		t.Errorf("read concurrency of a timed local disk %d", n)
	}
}

type retainingLocal struct{ Backend }

func (retainingLocal) LockDays() int { return 7 }
func (retainingLocal) KeepLocked(context.Context, []string) (int, error) {
	return 0, nil
}

// Storage that makes objects immutable keeps that ability when counted.
func TestTimedKeepsRetainer(t *testing.T) {
	local, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := NewTimed(retainingLocal{local}).Wrapped().(Retainer); !ok {
		t.Error("a counted S3 with Object Lock lost its lock")
	}
	if _, ok := NewTimed(local).Wrapped().(Retainer); ok {
		t.Error("a counted local disk claims to lock objects")
	}
}
