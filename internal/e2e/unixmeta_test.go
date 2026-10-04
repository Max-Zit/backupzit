//go:build linux

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
	"golang.org/x/sys/unix"
)

// TestUnixMetadata backs up and restores owner, setuid bits, extended
// attributes, hard links, named pipes and device nodes (the latter two and
// ownership only as root).
func TestUnixMetadata(t *testing.T) {
	ctx := context.Background()
	root := os.Geteuid() == 0
	src := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(src, "setuid"), []byte("x"), 0o755))
	must(unix.Chmod(filepath.Join(src, "setuid"), 0o4755))
	must(os.Mkdir(filepath.Join(src, "sticky"), 0o755))
	must(unix.Chmod(filepath.Join(src, "sticky"), 0o1777))
	must(os.WriteFile(filepath.Join(src, "a"), []byte("hard linked"), 0o644))
	must(os.Link(filepath.Join(src, "a"), filepath.Join(src, "b")))
	must(os.Symlink("a", filepath.Join(src, "link")))
	xattrOK := unix.Lsetxattr(filepath.Join(src, "a"), "user.backupzit", []byte("v1"), 0) == nil
	must(unix.Mkfifo(filepath.Join(src, "fifo"), 0o600))
	if root {
		must(os.Chown(filepath.Join(src, "a"), 1234, 2345))
		must(unix.Mknod(filepath.Join(src, "null"), unix.S_IFCHR|0o666, int(unix.Mkdev(1, 3))))
		must(unix.Lsetxattr(filepath.Join(src, "setuid"), "security.capability", []byte{1, 0, 0, 2, 0, 32, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0))
	}
	// A nested file system is simulated by OneFileSystem being a no-op
	// here; the mount handling is covered by the system backup test.

	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	must(err)
	r, err := repo.Init(ctx, be, repo.Password("k"))
	must(err)
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, OneFileSystem: true})
	must(err)
	if len(sn.Stats.Errors) > 0 {
		t.Fatalf("backup errors: %v", sn.Stats.Errors)
	}
	dst := t.TempDir()
	st, err := restorer.Run(ctx, r, sn, restorer.Options{Target: dst})
	must(err)
	if len(st.Errors) > 0 {
		t.Fatalf("restore errors: %v", st.Errors)
	}
	out := filepath.Join(dst, src)
	stat := func(p string) *syscall.Stat_t {
		t.Helper()
		var s syscall.Stat_t
		must(syscall.Lstat(filepath.Join(out, p), &s))
		return &s
	}
	if m := stat("setuid").Mode & 0o7777; m != 0o4755 {
		t.Errorf("setuid mode %o", m)
	}
	if m := stat("sticky").Mode & 0o7777; m != 0o1777 {
		t.Errorf("sticky mode %o", m)
	}
	if stat("a").Ino != stat("b").Ino {
		t.Error("hard link not restored")
	}
	if l, _ := os.Readlink(filepath.Join(out, "link")); l != "a" {
		t.Errorf("symlink %q", l)
	}
	if stat("fifo").Mode&unix.S_IFMT != unix.S_IFIFO {
		t.Error("fifo not restored")
	}
	if xattrOK {
		buf := make([]byte, 16)
		n, err := unix.Lgetxattr(filepath.Join(out, "a"), "user.backupzit", buf)
		if err != nil || !bytes.Equal(buf[:n], []byte("v1")) {
			t.Errorf("xattr: %q %v", buf[:n], err)
		}
	}
	if root {
		if s := stat("a"); s.Uid != 1234 || s.Gid != 2345 {
			t.Errorf("owner %d:%d", s.Uid, s.Gid)
		}
		if s := stat("null"); s.Mode&unix.S_IFMT != unix.S_IFCHR || s.Rdev != unix.Mkdev(1, 3) {
			t.Errorf("device node %o %d", s.Mode, s.Rdev)
		}
		buf := make([]byte, 64)
		if n, err := unix.Lgetxattr(filepath.Join(out, "setuid"), "security.capability", buf); err != nil || n != 20 {
			t.Errorf("file capability lost: %d %v", n, err)
		}
		if m := stat("setuid").Mode & 0o7777; m != 0o4755 {
			t.Errorf("setuid lost after chown: %o", m)
		}
	}
	t.Logf("root=%v xattr=%v", root, xattrOK)
}
