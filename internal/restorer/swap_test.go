package restorer_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

func folderLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Clean(link), filepath.Clean(target)).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
	}
	t.Skip("cannot create a link to a folder")
}

// swapSnapshot saves the folder "data" with the files a and b, nested
// below comps, and returns the repository and the root tree.
func swapSnapshot(t *testing.T, comps []string) (*repo.Repository, repo.ID) {
	t.Helper()
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	var files []repo.Node
	for _, n := range []string{"a", "b"} {
		data := []byte("restored " + n)
		blob, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, repo.Node{Name: n, Type: repo.NodeFile, Mode: 0o644, Size: uint64(len(data)), Content: []repo.ID{blob}})
	}
	id, err := r.SaveTree(ctx, &repo.Tree{Nodes: files})
	if err != nil {
		t.Fatal(err)
	}
	node := repo.Node{Name: "data", Type: repo.NodeDir, Mode: 0o750 | uint32(os.ModeDir), Subtree: &id, WinAttrs: 0x2} // hidden
	for i := len(comps) - 1; i >= 0; i-- {
		id, err = r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{node}})
		if err != nil {
			t.Fatal(err)
		}
		node = repo.Node{Name: comps[i], Type: repo.NodeDir, Mode: 0o755 | uint32(os.ModeDir), Subtree: &id}
	}
	root, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{node}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	return r, root
}

// 0.58: the folder a restore writes into, swapped for a link to another
// folder while the restore runs (after its first file), receives the rest
// of the files itself: nothing is written where the link points, and the
// folder's owner, mode and attributes are not set there either. Below a
// restore folder and at the original location.
func TestFolderSwappedForALinkDuringRestore(t *testing.T) {
	for _, original := range []bool{false, true} {
		name := "restore folder"
		if original {
			name = "original location"
		}
		t.Run(name, func(t *testing.T) {
			base := filepath.Clean(t.TempDir())
			outside := filepath.Join(filepath.Clean(t.TempDir()), "out")
			if err := os.Mkdir(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			var comps []string
			opts := restorer.Options{Target: base}
			dataDir := filepath.Join(base, "data")
			if original {
				c, err := fsutil.ParseSnapshotPath(base)
				if err != nil {
					t.Fatal(err)
				}
				comps, opts.Target = c, ""
			}
			r, root := swapSnapshot(t, comps)
			swapped := false
			opts.Progress = func(p string, _ *restorer.Stats) {
				if swapped || !strings.HasSuffix(p, string(filepath.Separator)+"a") {
					return
				}
				swapped = true
				if err := os.Rename(dataDir, dataDir+"2"); err != nil {
					t.Errorf("the folder being restored into cannot be moved away: %v", err)
					return
				}
				folderLink(t, outside, dataDir)
			}
			st, err := restorer.Run(context.Background(), r, &repo.Snapshot{Tree: root}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !swapped {
				t.Fatalf("no swap: %+v", st)
			}
			if names, _ := os.ReadDir(outside); len(names) != 0 {
				t.Fatalf("written through the link swapped in: %v", names)
			}
			for _, f := range []string{"a", "b"} {
				if b, err := os.ReadFile(filepath.Join(dataDir+"2", f)); err != nil || string(b) != "restored "+f {
					t.Errorf("%s in the folder that was opened: %q %v", f, b, err)
				}
			}
			fi, err := os.Stat(outside)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 {
				t.Errorf("the mode of the folder was set where the link points: %v", fi.Mode())
			}
			if runtime.GOOS == "windows" && winHidden(outside) {
				t.Error("the attributes of the folder were set where the link points")
			}
		})
	}
}
