package recovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// newImageRepo creates an encrypted repository with one image snapshot
// (a 3-partition GPT disk; partition 2 only in the layout) and one file
// snapshot.
func newImageRepo(t *testing.T, dir, password string) {
	t.Helper()
	ctx := context.Background()
	be, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password(password))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ids := make([]repo.ID, 16)
	for i := range ids {
		if i%4 == 3 {
			continue
		}
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, bytes.Repeat([]byte{byte(i)}, 4096))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	maps, err := r.SaveBlockMap(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := r.SaveBlob(ctx, repo.DataBlob, make([]byte, 512))
	tree, _ := r.SaveTree(ctx, &repo.Tree{})
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	img := repo.DiskImage{Number: 0, Size: 1 << 30, SectorSize: 512, Style: "gpt", GPTDiskID: "C6552194-B882-49DD-884D-1100B1A5FE52", Head: head,
		Partitions: []repo.PartitionImage{
			{Number: 1, Offset: 1 << 20, Length: 16 * 4096, Included: true, BlockSize: 4096, Blocks: 16, Maps: maps, StoredBytes: 12 * 4096},
			{Number: 2, Offset: 2 << 20, Length: 1 << 20},
			{Number: 3, Offset: 4 << 20, Length: 16 * 4096, Included: true, BlockSize: 4096, Blocks: 16, Maps: maps, StoredBytes: 12 * 4096},
		}}
	if _, err := r.SaveSnapshot(ctx, &repo.Snapshot{Hostname: "pc1", Tree: tree, Images: []repo.DiskImage{img}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveSnapshot(ctx, &repo.Snapshot{Hostname: "pc1", Tree: tree, Paths: []string{`C:\data`}}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAndListImages(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	newImageRepo(t, dir, "secret")

	if _, err := Open(ctx, Source{Location: dir, Password: "wrong"}); !errors.Is(err, ErrPassword) {
		t.Errorf("wrong password: got %v", err)
	}
	if _, err := Open(ctx, Source{Location: dir}); !errors.Is(err, ErrPassword) {
		t.Errorf("no password: got %v", err)
	}
	if _, err := Open(ctx, Source{}); err == nil {
		t.Error("empty location must fail")
	}
	s, err := Open(ctx, Source{Location: dir, Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sns, err := s.ImageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sns) != 1 || len(sns[0].Snapshot.Images) != 1 {
		t.Fatalf("want one image snapshot, got %d", len(sns))
	}

	j := Job{Snapshot: sns[0].Snapshot, Partitions: []int{3}}
	var last uint64
	if err := Verify(ctx, s.Repo, j, func(phase string, done, total uint64) { last = done }); err != nil {
		t.Fatal(err)
	}
	if last != 12*4096 {
		t.Errorf("verified %d bytes, want %d", last, 12*4096)
	}
	j.Partitions = nil
	if err := Verify(ctx, s.Repo, j, nil); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyFindsDamage(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	newImageRepo(t, dir, "pw")
	// Damage every data pack.
	err := filepath.Walk(filepath.Join(dir, "data"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i := 0; i < len(b); i += 64 {
			b[i] ^= 0xFF
		}
		return os.WriteFile(p, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, Source{Location: dir, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sns, err := s.ImageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, s.Repo, Job{Snapshot: sns[0].Snapshot}, nil); err == nil {
		t.Error("verification of a damaged repository must fail")
	}
}

func TestFindLocalRepos(t *testing.T) {
	root := t.TempDir()
	mk := func(p string, repoDir bool) {
		if err := os.MkdirAll(filepath.Join(root, p, "snapshots"), 0o700); err != nil {
			t.Fatal(err)
		}
		if repoDir {
			if err := os.WriteFile(filepath.Join(root, p, "config"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("backups/pc1", true)
	mk("backups/pc2", true)
	mk("deep/a/b/c/pc3", true)   // too deep
	mk("Windows/pc4", true)      // skipped folder
	mk("backups/nothing", false) // no config
	got := FindLocalRepos([]string{root}, 3)
	var rel []string
	for _, g := range got {
		r, _ := filepath.Rel(root, g)
		rel = append(rel, filepath.ToSlash(r))
	}
	if strings.Join(rel, ",") != "backups/pc1,backups/pc2" {
		t.Errorf("found %v", rel)
	}
	if got := FindLocalRepos([]string{filepath.Join(root, "backups", "pc1")}, 0); len(got) != 1 {
		t.Errorf("root itself is a repository: %v", got)
	}
}

func testDisks() []imaging.Disk {
	return []imaging.Disk{
		{Number: 0, Size: 512 << 30, SectorSize: 512, Style: imaging.StyleGPT, GPTDiskID: "AAAA0000-0000-0000-0000-000000000000",
			Partitions: []imaging.Partition{{Number: 1, MountPoints: []string{`C:\`}}}},
		{Number: 1, Size: 64 << 30, SectorSize: 512, Style: imaging.StyleMBR, Bus: "usb",
			Partitions: []imaging.Partition{{Number: 1, MountPoints: []string{`E:\`}}}},
		{Number: 2, Size: 2 << 20, SectorSize: 512, Style: imaging.StyleRaw},
		{Number: 3, Size: 512 << 30, SectorSize: 4096, Style: imaging.StyleRaw},
	}
}

func TestProtectedDisks(t *testing.T) {
	p := ProtectedDisks(testDisks(), []string{`E:\backupzit\recovery.json`, `X:\Windows\System32\agent.exe`, `smb://host/share`})
	if len(p) != 1 || p[1] == "" {
		t.Errorf("protected = %v, want disk 1", p)
	}
}

func TestJobCheck(t *testing.T) {
	sn := &repo.Snapshot{Images: []repo.DiskImage{{Style: "gpt", SectorSize: 512, GPTDiskID: "AAAA0000-0000-0000-0000-000000000000", Partitions: []repo.PartitionImage{
		{Number: 1, Offset: 1 << 20, Length: 100 << 20, Included: true},
		{Number: 2, Offset: 101 << 20, Length: 16 << 20},
	}}}}
	disks := testDisks()
	prot := ProtectedDisks(disks, []string{`E:\x`})
	cases := []struct {
		job  Job
		want string
	}{
		{Job{Snapshot: sn, TargetDisk: 0}, ""},
		{Job{Snapshot: sn, TargetDisk: 1}, "cannot be the target"},
		{Job{Snapshot: sn, TargetDisk: 2}, "too small"},
		{Job{Snapshot: sn, TargetDisk: 3}, "byte sectors"},
		{Job{Snapshot: sn, TargetDisk: 9}, "not found"},
		{Job{Snapshot: sn, TargetDisk: 0, Partitions: []int{2}}, "not backed up"},
		{Job{Snapshot: sn, TargetDisk: 0, Partitions: []int{7}}, "no partition 7"},
		{Job{Snapshot: sn, TargetDisk: 0, Partitions: []int{}}, "no partition selected"},
		{Job{Snapshot: sn, TargetDisk: 0, Image: 1}, "no disk image"},
		{Job{TargetDisk: 0}, "no snapshot"},
	}
	for _, c := range cases {
		err := c.job.Check(disks, prot)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%+v: unexpected %v", c.job, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%+v: got %v, want %q", c.job, err, c.want)
		}
	}
	if !SameDiskAsSource(&sn.Images[0], disks[0]) {
		t.Error("same GPT disk ID should be recognised")
	}
	if SameDiskAsSource(&sn.Images[0], disks[1]) {
		t.Error("different disk recognised as source")
	}
}

func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	var lines []string
	l := NewLogger(&buf, func(s string) { lines = append(lines, s) })
	l.Printf("hello %d", 1)
	if !strings.Contains(buf.String(), "hello 1") || len(lines) != 1 {
		t.Errorf("log %q, %v", buf.String(), lines)
	}
	var nl *Logger
	nl.Printf("no panic")
}

func TestJobCheckLayout(t *testing.T) {
	sn := &repo.Snapshot{Images: []repo.DiskImage{{Style: "gpt", SectorSize: 512, Partitions: []repo.PartitionImage{
		{Number: 1, Offset: 1 << 20, Length: 100 << 20, Included: true, GPTType: imaging.GPTTypeEFI},
		{Number: 2, Offset: 101 << 20, Length: 10 << 30, Included: true, FileSystem: "NTFS"},
	}}}}
	img := &sn.Images[0]
	disks := testDisks()
	lim := map[int]imaging.PartitionLimits{2: {Resizable: true, MinSize: 5 << 30}}
	l, err := imaging.SuggestLayout(img, disks[0].Size, lim, imaging.LayoutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := (Job{Snapshot: sn, TargetDisk: 0, Layout: l}).Check(disks, nil); err != nil {
		t.Errorf("valid layout: %v", err)
	}
	if err := (Job{Snapshot: sn, TargetDisk: 0, Layout: l, Partitions: []int{2}}).Check(disks, nil); err == nil {
		t.Error("layout plus partition list accepted")
	}
	big, _ := imaging.SuggestLayout(img, 1<<40, lim, imaging.LayoutOptions{})
	if err := (Job{Snapshot: sn, TargetDisk: 0, Layout: big}).Check(disks, nil); err == nil {
		t.Error("layout for a larger disk accepted")
	}
	none := *l
	none.Partitions = append([]imaging.LayoutPartition(nil), l.Partitions...)
	for i := range none.Partitions {
		none.Partitions[i].Restore = false
	}
	if err := (Job{Snapshot: sn, TargetDisk: 0, Layout: &none}).Check(disks, nil); err == nil {
		t.Error("layout without restored partitions accepted")
	}
	j := Job{Snapshot: sn, Layout: l}
	if got := j.restoredPartitions(); len(got) != 2 {
		t.Errorf("restored partitions %v", got)
	}
}
