package server

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// In a backup of containers a symbolic link stays in its volume: an
// absolute target is inside the container, and no link reaches the files
// of another container.
func TestFollowLinkStaysInContainerVolume(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	tree := func(nodes ...repo.Node) *repo.ID {
		slices.SortFunc(nodes, func(a, b repo.Node) int { return strings.Compare(a.Name, b.Name) })
		id, err := r.SaveTree(ctx, &repo.Tree{Nodes: nodes})
		if err != nil {
			t.Fatal(err)
		}
		return &id
	}
	dir := func(name string, nodes ...repo.Node) repo.Node {
		return repo.Node{Name: name, Type: repo.NodeDir, Mode: uint32(os.ModeDir | 0o755), Subtree: tree(nodes...)}
	}
	link := func(name, target string) repo.Node {
		return repo.Node{Name: name, Type: repo.NodeSymlink, Mode: uint32(os.ModeSymlink | 0o777), LinkTarget: target}
	}
	file := repo.Node{Name: "hostname", Type: repo.NodeFile, Mode: 0o644}
	secret := repo.Node{Name: "shadow", Type: repo.NodeFile, Mode: 0o600}
	root := tree(dir("lxc",
		dir("200", dir("rootfs", dir("etc", file,
			link("abs", "/etc/hostname"),
			link("rel", "../etc/hostname"),
			link("other", "../../../300/rootfs/etc/shadow"),
			link("up", "../../../../../lxc/300/rootfs/etc/shadow"),
			link("drive", "C:/etc/hostname"),
		))),
		dir("300", dir("rootfs", dir("etc", secret))),
	))
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	follow := func(name string, containers bool) (*repo.Node, error) {
		comps := []string{"lxc", "200", "rootfs", "etc", name}
		_, n, err := lookupNode(ctx, r, *root, comps)
		if err != nil {
			t.Fatal(err)
		}
		return followLink(ctx, r, *root, comps, n, containers)
	}
	for _, name := range []string{"abs", "rel"} {
		if n, err := follow(name, true); err != nil || n.Name != "hostname" {
			t.Errorf("%s: %v %v", name, n, err)
		}
	}
	for _, name := range []string{"other", "up", "drive"} {
		if n, err := follow(name, true); err == nil {
			t.Errorf("%s left its container volume: reached %s", name, n.Name)
		} else if name != "drive" && !strings.Contains(err.Error(), "outside its container volume") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// File backups keep resolving against the root of the snapshot.
	if n, err := follow("other", false); err != nil || n.Name != "shadow" {
		t.Errorf("file backup link: %v %v", n, err)
	}
}
