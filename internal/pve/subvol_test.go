package pve

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// subvolFixture is a repository and a fake ZFS subvolume whose snapshot
// directory the test fills.
type subvolFixture struct {
	r      *repo.Repository
	be     backend.Backend
	mp     string // mountpoint of the dataset
	target string // mountpoint of the subvolume allocated on restore
	st     storage
}

func newSubvolFixture(t *testing.T) *subvolFixture {
	t.Helper()
	ctx := context.Background()
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	f := &subvolFixture{
		r: r, be: be, mp: t.TempDir(), target: t.TempDir(),
		st: storage{ID: "local-zfs", Type: "zfspool", Props: map[string]string{"pool": "rpool/data"}},
	}
	old := command
	t.Cleanup(func() { command = old })
	command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "zfs" && args[0] == "get":
			if ds := args[len(args)-1]; ds != "rpool/data/subvol-201-disk-0" {
				t.Errorf("zfs get on %s", ds)
			}
			return []byte("mountpoint\t" + f.mp + "\nmounted\tyes\n"), nil
		case name == "pvesm" && args[0] == "path":
			return []byte(f.target + "\n"), nil
		}
		t.Fatalf("unexpected command %s %v", name, args)
		return nil, nil
	}
	return f
}

func (f *subvolFixture) write(t *testing.T, snap, name, data string) {
	t.Helper()
	p := filepath.Join(f.mp, ".zfs", "snapshot", snap, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unchanged files keep their time in the next ZFS snapshot.
	if err := os.Chtimes(p, fileTime, fileTime); err != nil {
		t.Fatal(err)
	}
}

var fileTime = time.Unix(1790000000, 0)

// container writes the files of a small container root file system.
func (f *subvolFixture) container(t *testing.T, snap, big string) {
	t.Helper()
	f.write(t, snap, "etc/hostname", "storage02\n")
	f.write(t, snap, "var/lib/data.bin", big)
	root := filepath.Join(f.mp, ".zfs", "snapshot", snap)
	if err := os.Symlink("../usr/bin", filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "etc/hostname"), filepath.Join(root, "etc/hostname.link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "tmp"), 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
}

// backup stores the container's rootfs from snapshot snap and saves a
// repository snapshot; it returns the snapshot and the bytes read.
func (f *subvolFixture) backup(t *testing.T, snap string) (*repo.Snapshot, uint64, []string) {
	t.Helper()
	ctx := context.Background()
	var read, done uint64
	run := guestRun{
		prevVolumes: previousVolumes(ctx, f.r), volumes: map[string]repo.Node{},
		progress: func(n uint64) { done += n }, read: &read, log: func(string, ...any) {},
	}
	g := Guest{VMID: 201, Type: "lxc", Name: "storage02"}
	d := diskEntry{Key: "rootfs", Volume: "local-zfs:subvol-201-disk-0", Opts: "size=8G"}
	gd, warns, err := run.backupVolumeFiles(ctx, f.r, g, d, f.st, "subvol-201-disk-0", snap)
	if err != nil {
		t.Fatal(err)
	}
	if gd.Path != "lxc/201/rootfs" || gd.Format != "subvol" || gd.Image != -1 || gd.Size != 8<<30 || !gd.Files() {
		t.Fatalf("guest disk = %+v", gd)
	}
	if done != gd.Size {
		t.Errorf("progress = %d, want the configured size %d", done, gd.Size)
	}
	gi := repo.Guest{Platform: "proxmox", Type: "lxc", VMID: 201, Name: g.Name, Config: "rootfs: local-zfs:subvol-201-disk-0,size=8G\n", Disks: []repo.GuestDisk{gd}}
	tree, err := saveVolumeTree(ctx, f.r, []repo.Guest{gi}, run.volumes)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sn := &repo.Snapshot{Time: time.Now().UTC(), Hostname: "pve", Tree: tree, Guests: []repo.Guest{gi}}
	if _, err := f.r.SaveSnapshot(ctx, sn); err != nil {
		t.Fatal(err)
	}
	return sn, read, warns
}

func TestVolumeFilesBackupRestore(t *testing.T) {
	skipOnWindows(t)
	ctx := context.Background()
	f := newSubvolFixture(t)
	big := strings.Repeat("container data\n", 10000)
	f.container(t, "backupzit_1", big)

	sn1, read1, warns := f.backup(t, "backupzit_1")
	if len(warns) > 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if read1 < uint64(len(big)) {
		t.Errorf("first backup read %d bytes", read1)
	}
	if sn1.Guests[0].Disks[0].Bytes != uint64(len(big)+2*len("storage02\n")) {
		t.Errorf("bytes = %d", sn1.Guests[0].Disks[0].Bytes)
	}

	// The next backup reads only the new file.
	f.container(t, "backupzit_2", big)
	f.write(t, "backupzit_2", "etc/motd", "hello\n")
	sn2, read2, _ := f.backup(t, "backupzit_2")
	if read2 != uint64(len("hello\n")) {
		t.Errorf("second backup read %d bytes, want only the new file", read2)
	}

	if res, err := checker.Run(ctx, f.r, checker.Options{ReadData: true}); err != nil || !res.OK() {
		t.Fatalf("check: %v %v", err, res.Errors)
	}

	// Forgetting the first backup keeps the data the second one shares.
	lock, err := f.r.Lock(ctx, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.RemoveSnapshot(ctx, sn1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Prune(ctx, repo.PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	r, err := repo.Open(ctx, f.be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sn, err := r.LoadSnapshot(ctx, sn2.ID.String())
	if err != nil {
		t.Fatal(err)
	}

	var done uint64
	if err := writeVolumeFiles(ctx, r, sn, sn.Guests[0].Disks[0], "local-zfs:subvol-202-disk-0", func(n uint64) { done += n }); err != nil {
		t.Fatal(err)
	}
	if done != sn.Guests[0].Disks[0].Bytes {
		t.Errorf("restore progress = %d of %d", done, sn.Guests[0].Disks[0].Bytes)
	}
	for name, want := range map[string]string{"etc/hostname": "storage02\n", "etc/motd": "hello\n", "var/lib/data.bin": big} {
		got, err := os.ReadFile(filepath.Join(f.target, name))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", name, got[:min(len(got), 20)], err)
		}
	}
	if l, err := os.Readlink(filepath.Join(f.target, "bin")); err != nil || l != "../usr/bin" {
		t.Errorf("symlink: %q, %v", l, err)
	}
	a, _ := os.Stat(filepath.Join(f.target, "etc/hostname"))
	b, _ := os.Stat(filepath.Join(f.target, "etc/hostname.link"))
	if a == nil || b == nil || !os.SameFile(a, b) {
		t.Error("hard link not restored")
	}
	if fi, err := os.Stat(filepath.Join(f.target, "tmp")); err != nil || fi.Mode()&os.ModeSticky == 0 {
		t.Errorf("tmp: %v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(f.target, "var/lib")); err != nil || !fi.ModTime().Equal(dirTime(t, f, "var/lib")) {
		t.Errorf("var/lib time: %v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(f.target, "var")); err != nil || !fi.ModTime().Equal(dirTime(t, f, "var")) {
		t.Errorf("first level folder time not restored: %v, %v", fi, err)
	}
}

func dirTime(t *testing.T, f *subvolFixture, p string) time.Time {
	t.Helper()
	fi, err := os.Stat(filepath.Join(f.mp, ".zfs", "snapshot", "backupzit_2", p))
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func TestVolumeFilesCheckMissingPath(t *testing.T) {
	skipOnWindows(t)
	ctx := context.Background()
	f := newSubvolFixture(t)
	f.write(t, "backupzit_1", "etc/hostname", "ct\n")
	sn, _, _ := f.backup(t, "backupzit_1")
	bad := *sn
	bad.ID = repo.ID{}
	bad.Guests = []repo.Guest{sn.Guests[0]}
	bad.Guests[0].Disks = []repo.GuestDisk{sn.Guests[0].Disks[0]}
	bad.Guests[0].Disks[0].Path = "lxc/999/rootfs"
	if _, err := f.r.SaveSnapshot(ctx, &bad); err != nil {
		t.Fatal(err)
	}
	res, err := checker.Run(ctx, f.r, checker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() || !strings.Contains(strings.Join(res.Errors, "\n"), "volume rootfs") {
		t.Errorf("missing volume not reported: %v", res.Errors)
	}
}

// A backup with VMs and containers on ZFS: the restore test reads blocks of
// the disks and restores files of the containers.
func TestVolumeFilesSampleVerify(t *testing.T) {
	skipOnWindows(t)
	ctx := context.Background()
	f := newSubvolFixture(t)
	f.write(t, "backupzit_1", "etc/hostname", "ct\n")
	sn, _, _ := f.backup(t, "backupzit_1")
	disk := bytes.Repeat([]byte{0x5a}, diskimg.BlockSize)
	img, err := diskimg.Store(ctx, f.r, bytes.NewReader(disk), uint64(len(disk)), 0, "vm", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sn.Images = []repo.DiskImage{img}
	st, err := restorer.SampleVerify(ctx, f.r, sn, restorer.SampleOptions{Files: 10, MaxBytes: 1 << 20, Blocks: 10, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocks != 1 || st.Files != 1 || st.FileCandidates != 1 || len(st.Errors) > 0 {
		t.Errorf("sample = %+v", st)
	}
}

func TestVolumeFormatSubvol(t *testing.T) {
	zfs := storage{Type: "zfspool"}
	for vol, want := range map[string]bool{
		"subvol-201-disk-0":                    true,
		"basevol-100-disk-0":                   true,
		"basevol-100-disk-0/subvol-101-disk-0": true,
		"vm-100-disk-0":                        false,
	} {
		if got := isFilesVolume(zfs, vol); got != want {
			t.Errorf("%s: files = %v", vol, got)
		}
	}
	if isFilesVolume(storage{Type: "dir"}, "201/subvol-201-disk-0.subvol") {
		t.Error("directory storage volume treated as ZFS")
	}
	if got := subvolDataset(storage{Props: map[string]string{"pool": "rpool/data"}}, "basevol-100-disk-0/subvol-101-disk-0"); got != "rpool/data/subvol-101-disk-0" {
		t.Errorf("dataset = %s", got)
	}
}

// skipOnWindows: these tests build a fake ZFS mountpoint with Unix paths and
// symlinks (Proxmox VE runs on Linux; Windows needs a privilege for symlinks).
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux only: fake ZFS mountpoint with Unix paths and symlinks")
	}
}
