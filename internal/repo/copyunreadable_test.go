package repo_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// MED 11a: a source backup that cannot be read is left out and named;
// the backups before and after it are still copied, complete, also when
// the unreadable one shares data with them. Only when none can be read
// does the copy fail.
func TestCopySkipsUnreadableSnapshot(t *testing.T) {
	ctx := context.Background()
	src, _, sns, files := copySource(t)

	// A backup with the first backup's data plus a chunk that is missing:
	// found unreadable only after most of its blobs were looked at.
	tr0, err := src.LoadTree(ctx, sns[0].Tree)
	if err != nil {
		t.Fatal(err)
	}
	node := tr0.Nodes[0]
	node.Content = append(append([]repo.ID{}, node.Content...), repo.ID{9, 9, 9})
	badTree, err := src.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{node}})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	missingData := &repo.Snapshot{ID: repo.ID{1}, Time: time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC), Hostname: "h", Tree: badTree}
	// And one whose folder list is gone.
	missingTree := *sns[1]
	missingTree.Tree = repo.ID{2, 2, 2}

	dlocal, err := backend.OpenLocal(filepath.Join(t.TempDir(), "dst"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := repo.Init(ctx, dlocal)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	st, copied, err := repo.Copy(ctx, src, dst, []*repo.Snapshot{missingData, sns[0], &missingTree, sns[2]}, []string{"job:9"}, nil)
	if err != nil {
		t.Fatalf("copy failed instead of leaving the unreadable backups out: %v", err)
	}
	if st.Snapshots != 2 || len(copied) != 2 {
		t.Fatalf("%d backups copied, want 2", st.Snapshots)
	}
	if len(st.Unreadable) != 2 || !strings.Contains(st.Unreadable[0], missingData.ID.Short()) || !strings.Contains(st.Unreadable[1], missingTree.ID.Short()) {
		t.Errorf("unreadable: %q", st.Unreadable)
	}
	for i, want := range [][]byte{files[0], files[2]} {
		if got := readFile(t, dst, copied[i]); !bytes.Equal(got, want) {
			t.Errorf("copy %d reads back wrong (%d bytes, want %d)", i, len(got), len(want))
		}
	}

	// Nothing readable: the copy fails as before, and stores nothing.
	dlocal2, err := backend.OpenLocal(filepath.Join(t.TempDir(), "dst2"))
	if err != nil {
		t.Fatal(err)
	}
	dst2, err := repo.Init(ctx, dlocal2)
	if err != nil {
		t.Fatal(err)
	}
	defer dst2.Close()
	if _, copied, err := repo.Copy(ctx, src, dst2, []*repo.Snapshot{&missingTree}, nil, nil); err == nil || len(copied) != 0 {
		t.Fatalf("only unreadable backups: %v, %d copied", err, len(copied))
	}
}
