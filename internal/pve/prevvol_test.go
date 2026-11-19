package pve

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// Unchanged files are taken only from earlier backups of the same host:
// another host's container 100 is another machine.
func TestPreviousVolumesOfThisHostOnly(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	dir := func(name string, nodes ...repo.Node) repo.Node {
		id, err := r.SaveTree(ctx, &repo.Tree{Nodes: nodes})
		if err != nil {
			t.Fatal(err)
		}
		return repo.Node{Name: name, Type: repo.NodeDir, Mode: uint32(os.ModeDir | 0o755), Subtree: &id}
	}
	root := dir("", dir("lxc", dir("100", dir("rootfs"))))
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	g := repo.Guest{Platform: "proxmox", Type: "lxc", VMID: 100, Disks: []repo.GuestDisk{{Key: "rootfs", Format: "subvol", Image: -1, Path: "lxc/100/rootfs"}}}
	if _, err := r.SaveSnapshot(ctx, &repo.Snapshot{Time: time.Now().UTC(), Hostname: "other", Tree: *root.Subtree, Guests: []repo.Guest{g}}); err != nil {
		t.Fatal(err)
	}
	if prev := previousVolumes(ctx, r, "pve1"); prev["lxc/100/rootfs"] != nil {
		t.Fatal("another host's container volume used as the previous backup")
	}
	if prev := previousVolumes(ctx, r, "other"); prev["lxc/100/rootfs"] == nil {
		t.Fatal("the host's own previous backup not found")
	}
}

// A dataset that is not mounted leaves its mount point as a plain folder
// of the file system above: nothing may be restored into it.
func TestMountedDirRefusesPlainFolder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mount points are checked on Linux")
	}
	if mountedDir(t.TempDir()) {
		t.Error("a plain folder counts as a mounted subvolume")
	}
	if !mountedDir("/proc") {
		t.Error("/proc does not count as mounted")
	}
}
