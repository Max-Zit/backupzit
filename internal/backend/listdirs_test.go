package backend

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// listOnly hides ListDirs, as on storage that can only list everything.
type listOnly struct{ Backend }

// The folders below a storage location that hold a repository are found
// with ListDirs and, where storage has no folders, from a full listing.
func TestRepositoryDirs(t *testing.T) {
	ctx := context.Background()
	l, err := OpenLocal(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pc-a_11111111/config", "pc-a_11111111/data/0a/0a11", "pc-b_22222222/config", "other/notes.txt", "top.txt"} {
		if err := l.Save(ctx, n, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"pc-a_11111111", "pc-b_22222222"}
	for name, be := range map[string]Backend{"folders": l, "listing": listOnly{l}} {
		got, err := RepositoryDirs(ctx, be)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
}
