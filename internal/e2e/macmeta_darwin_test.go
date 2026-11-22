//go:build darwin

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
	"golang.org/x/sys/unix"
)

// setCrtime sets the creation time of p (not following a link).
func setCrtime(t *testing.T, p string, bt time.Time) {
	t.Helper()
	al := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME}
	ts := unix.NsecToTimespec(bt.UnixNano())
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&ts)), unsafe.Sizeof(ts)) // #nosec G103 -- setattrlist buffer
	if err := unix.Setattrlist(p, &al, buf, unix.FSOPT_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
}

// TestMacMetadata backs up and restores the BSD file flags (hidden,
// nodump, immutable, append-only) and creation times of files, folders,
// links and named pipes; a compressed file comes back as an ordinary file with the same
// content; a second restore replaces the immutable file of the first.
func TestMacMetadata(t *testing.T) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	src := t.TempDir()
	in := func(p string) string { return filepath.Join(src, p) }
	bt := time.Date(2003, 4, 5, 6, 7, 8, 123456789, time.UTC)
	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	must(os.WriteFile(in("hidden"), []byte("h"), 0o644))
	must(os.WriteFile(in("nodump"), []byte("n"), 0o644))
	must(os.WriteFile(in("locked"), []byte("l"), 0o444))
	must(os.WriteFile(in("append"), []byte("a"), 0o644))
	must(os.Mkdir(in("hiddendir"), 0o755))
	must(os.WriteFile(in("hiddendir/inside"), []byte("i"), 0o644))
	must(os.Symlink("hidden", in("link")))
	must(unix.Mkfifo(in("fifo"), 0o600))
	for _, p := range []string{"hidden", "nodump", "locked", "append", "hiddendir/inside", "hiddendir", "fifo"} {
		must(os.Chtimes(in(p), mt, mt))
		setCrtime(t, in(p), bt)
	}
	setCrtime(t, in("link"), bt.Add(time.Hour))
	must(unix.Chflags(in("hidden"), unix.UF_HIDDEN))
	must(unix.Chflags(in("nodump"), unix.UF_NODUMP))
	must(unix.Chflags(in("hiddendir"), unix.UF_HIDDEN))
	must(unix.Chflags(in("locked"), unix.UF_IMMUTABLE|unix.UF_HIDDEN))
	must(unix.Chflags(in("append"), unix.UF_APPEND))
	must(unix.Chflags(in("fifo"), unix.UF_HIDDEN|unix.UF_NODUMP))
	// A link's own flag, through a handle of the link.
	lfd, err := unix.Open(in("link"), unix.O_RDONLY|unix.O_SYMLINK, 0)
	must(err)
	must(unix.Fchflags(lfd, unix.UF_HIDDEN))
	_ = unix.Close(lfd)
	t.Cleanup(func() {
		for _, p := range []string{"locked", "append"} {
			_ = unix.Chflags(in(p), 0)
		}
	})

	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	must(err)
	r, err := repo.Init(ctx, be, repo.Password("k"))
	must(err)
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	must(err)
	if len(sn.Stats.Errors) > 0 {
		t.Fatalf("backup errors: %v", sn.Stats.Errors)
	}
	dst := t.TempDir()
	out := func(p string) string { return filepath.Join(dst, src, p) }
	t.Cleanup(func() {
		for _, p := range []string{"locked", "append"} {
			_ = unix.Chflags(out(p), 0)
		}
	})
	for round := 1; round <= 2; round++ {
		st, err := restorer.Run(ctx, r, sn, restorer.Options{Target: dst, Verify: true})
		must(err)
		if len(st.Errors) > 0 {
			t.Fatalf("restore %d errors: %v", round, st.Errors)
		}
	}

	for p, want := range map[string]uint32{
		"hidden": unix.UF_HIDDEN, "nodump": unix.UF_NODUMP, "hiddendir": unix.UF_HIDDEN,
		"locked": unix.UF_IMMUTABLE | unix.UF_HIDDEN, "append": unix.UF_APPEND, "link": unix.UF_HIDDEN,
		"hiddendir/inside": 0, "fifo": unix.UF_HIDDEN | unix.UF_NODUMP,
	} {
		if got := flagsOf(t, out(p)); got != want {
			t.Errorf("%s: flags %#x, want %#x", p, got, want)
		}
	}
	for p, want := range map[string]time.Time{
		"hidden": bt, "nodump": bt, "locked": bt, "hiddendir": bt, "hiddendir/inside": bt, "fifo": bt, "link": bt.Add(time.Hour),
	} {
		var s unix.Stat_t
		must(unix.Lstat(out(p), &s))
		if got := time.Unix(s.Btim.Unix()).UTC(); !got.Equal(want) {
			t.Errorf("%s: creation time %v, want %v", p, got, want)
		}
		if p != "link" {
			if got := time.Unix(s.Mtim.Unix()).UTC(); !got.Equal(mt) {
				t.Errorf("%s: modification time %v, want %v", p, got, mt)
			}
		}
	}
	if l, _ := os.Readlink(out("link")); l != "hidden" {
		t.Errorf("symlink %q", l)
	}
	if flagsOf(t, in("hidden")) != unix.UF_HIDDEN {
		t.Error("the link's flag was set on its target")
	}
}

// TestMacCompressedFile backs up a file compressed by the file system
// (decmpfs: /bin/ls of the system volume) and restores it as an ordinary
// file with the same content: the flag is stored, the compressed data and
// its attribute are not (the kernel hides them), and the restore does not
// set the flag again.
func TestMacCompressedFile(t *testing.T) {
	const ls = "/bin/ls"
	ctx := context.Background()
	var s unix.Stat_t
	if err := unix.Lstat(ls, &s); err != nil || s.Flags&unix.UF_COMPRESSED == 0 {
		t.Skipf("%s is not compressed here (%v)", ls, err)
	}
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password("k"))
	if err != nil {
		t.Fatal(err)
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{ls}})
	if err != nil || len(sn.Stats.Errors) > 0 {
		t.Fatalf("backup: %v %v", err, sn)
	}
	n := findNode(ctx, t, r, sn.Tree, ls)
	if n.Unix == nil || n.Unix.Flags&unix.UF_COMPRESSED == 0 {
		t.Fatalf("flags not stored: %+v", n.Unix)
	}
	for name := range n.Unix.Xattrs {
		if name == "com.apple.decmpfs" || name == "com.apple.ResourceFork" {
			t.Errorf("compression data stored as attribute %s", name)
		}
	}
	dst := t.TempDir()
	st, err := restorer.Run(ctx, r, sn, restorer.Options{Target: dst, Verify: true})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("restore: %v %v", err, st)
	}
	out := filepath.Join(dst, ls)
	if f := flagsOf(t, out); f&unix.UF_COMPRESSED != 0 {
		t.Errorf("restored file marked compressed: %#x", f)
	}
	a, _ := os.ReadFile(ls)
	b, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(a, b) {
		t.Errorf("compressed file restored with other content (%v)", err)
	}
}

// findNode returns the node of the original path p in the snapshot tree.
func findNode(ctx context.Context, t *testing.T, r *repo.Repository, tree repo.ID, p string) repo.Node {
	t.Helper()
	comps, err := fsutil.SnapshotComponents(p)
	if err != nil {
		t.Fatal(err)
	}
	id := tree
	for i, c := range comps {
		tr, err := r.LoadTree(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		n := tr.Find(c)
		if n == nil {
			t.Fatalf("%s: %q not in the snapshot", p, c)
		}
		if i == len(comps)-1 {
			return *n
		}
		if n.Subtree == nil {
			t.Fatalf("%s: %q is not a folder", p, c)
		}
		id = *n.Subtree
	}
	t.Fatalf("%s: empty path", p)
	return repo.Node{}
}

func flagsOf(t *testing.T, p string) uint32 {
	t.Helper()
	var s unix.Stat_t
	if err := unix.Lstat(p, &s); err != nil {
		t.Fatal(err)
	}
	return s.Flags
}
