package vmfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"
)

// dirLink makes link point to the folder target: a symbolic link, or on
// Windows without the right to create one a junction (which any user may
// create).
func dirLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
	}
	t.Skip("cannot create a link to a folder")
}

// B12 (round 4): a link or junction put into the restore folder sent the
// restored files to where it pointed, with the agent's rights. Folders
// below the restore folder are now always real folders.
func TestExtractDoesNotFollowFolderLinksInTarget(t *testing.T) {
	f := &ioFS{fsys: fstest.MapFS{
		"edge/comp/a.txt": {Data: []byte("aaa"), Mode: 0o644},
		"edge/b.txt":      {Data: []byte("bbb"), Mode: 0o644},
	}}
	for _, at := range []string{"edge", filepath.Join("edge", "comp")} {
		t.Run(at, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, at)), 0o755); err != nil {
				t.Fatal(err)
			}
			dirLink(t, outside, filepath.Join(dir, at))
			st, err := Extract(context.Background(), f, []string{"/edge"}, dir)
			if err != nil || len(st.Errors) > 0 {
				t.Fatal(err, st.Errors)
			}
			if names, _ := os.ReadDir(outside); len(names) > 0 {
				t.Fatalf("written through the link: %v", names)
			}
			fi, err := os.Lstat(filepath.Join(dir, at))
			if err != nil || !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				t.Fatalf("%s is not a real folder: %v %v", at, fi.Mode(), err)
			}
			for p, want := range map[string]string{"edge/comp/a.txt": "aaa", "edge/b.txt": "bbb"} {
				if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))); err != nil || string(b) != want {
					t.Errorf("%s: %q %v", p, b, err)
				}
			}
		})
	}
}

// B12: a symbolic link at the temporary name of a restored file had its
// target overwritten with the restored data.
func TestExtractDoesNotWriteThroughPartFileLink(t *testing.T) {
	f := &ioFS{fsys: fstest.MapFS{"etc/hostname": {Data: []byte("from the backup"), Mode: 0o644}}}
	dir, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "etc", "hostname.bzpart")); err != nil {
		t.Skipf("cannot create a symbolic link: %v", err)
	}
	st, err := Extract(context.Background(), f, []string{"/etc/hostname"}, dir)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st.Errors)
	}
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("victim overwritten: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(dir, "etc", "hostname"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("restored file is a link: %v %v", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "etc", "hostname")); string(b) != "from the backup" {
		t.Errorf("content %q", b)
	}
}
