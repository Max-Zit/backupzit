package backend

import (
	"context"
	"errors"
	"testing"
)

// L-27 (0.55): removing a missing file reports ErrNotFound, as on the
// other backends, so prune and lock removal treat it as already gone.
func TestLocalRemoveMissing(t *testing.T) {
	l, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Remove(context.Background(), "data/ab/abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove of a missing file: %v", err)
	}
}
