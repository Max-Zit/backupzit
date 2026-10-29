package e2e

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// A wildcard source that finds one more folder (a new user) keeps the job's
// previous backup as parent: the files already backed up are not read again.
func TestParentOfSameJobAfterNewFolder(t *testing.T) {
	ctx := context.Background()
	root := tempDir(t)
	be, err := backend.OpenLocal(filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(root, "Users")
	writeFile(t, filepath.Join(users, "ana", "Documents", "big.bin"), bytes.Repeat([]byte("a"), 4<<20))
	opts := func(paths ...string) archiver.Options {
		return archiver.Options{Paths: paths, Version: "test", Tags: []string{"run:1", "job:7"}}
	}
	if _, err := archiver.Run(ctx, r, opts(filepath.Join(users, "ana", "Documents"))); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(users, "marko", "Documents", "new.txt"), []byte("new user"))
	sn, err := archiver.Run(ctx, r, opts(filepath.Join(users, "ana", "Documents"), filepath.Join(users, "marko", "Documents")))
	if err != nil {
		t.Fatal(err)
	}
	if sn.Parent == nil {
		t.Fatal("no parent: the previous backup of the job was not used")
	}
	if sn.Stats.BytesRead != uint64(len("new user")) {
		t.Fatalf("read %d bytes, want only the new user's file", sn.Stats.BytesRead)
	}
}
