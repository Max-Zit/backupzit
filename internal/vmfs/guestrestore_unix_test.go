//go:build !windows

package vmfs

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/fstest"
)

// allocated returns the bytes path takes on disk.
func allocated(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}

// MED 17 (0.63): a folder of the guest was created 0755 and got its own
// permissions only after everything below it was written, so the files of
// a private folder were open to everyone for as long as it restored. It is
// now 0700 until then; folders above the selection keep 0755.
func TestExtractFoldersPrivateWhileWritten(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	dir := t.TempDir()
	perm := func(p string) fs.FileMode {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	var during map[string]fs.FileMode
	f := hookFS{FS: &ioFS{fsys: fstest.MapFS{
		"srv":            {Mode: fs.ModeDir | 0o751},
		"srv/app":        {Mode: fs.ModeDir | 0o750},
		"srv/app/secret": {Data: []byte("s"), Mode: 0o644},
	}}, before: func(p string) {
		if p == "/srv/app/secret" {
			during = map[string]fs.FileMode{"srv": perm("srv"), "srv/app": perm("srv/app")}
		}
	}}
	st, err := Extract(context.Background(), f, []string{"/srv/app"}, dir)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st.Errors)
	}
	if during["srv"] != 0o755 || during["srv/app"] != 0o700 {
		t.Errorf("while the file was written: srv %v (want 0755), srv/app %v (want 0700)", during["srv"], during["srv/app"])
	}
	if got := perm("srv/app"); got != 0o750 {
		t.Errorf("srv/app ends with %v, want 0750", got)
	}
	if got := perm("srv"); got != 0o755 {
		t.Errorf("srv (above the selection) ends with %v, want 0755", got)
	}
}
