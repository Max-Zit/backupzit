package restorer_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

var procGetCompressedFileSize = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// allocated returns the bytes path takes on disk (sparse ranges excluded).
func allocated(t *testing.T, path string) int64 {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xFFFFFFFF && callErr != windows.ERROR_SUCCESS {
		t.Fatal(callErr)
	}
	return int64(high)<<32 | int64(uint32(low))
}

// B9 (round 4): on Windows the skipped zero chunks were filled with zeros
// on disk, because NTFS leaves holes only in a file marked sparse. A 1 GiB
// sparse file came back taking 1 GiB.
func TestRestoreKeepsFilesSparseOnWindows(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 4<<20)
	data := bytes.Repeat([]byte("data"), 1024)
	z, _, err := r.SaveBlob(ctx, repo.DataBlob, zeros)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	content := []repo.ID{z, z, z, z, d, z, z, z, z}
	size := uint64(8*len(zeros) + len(data))
	// disk.img was sparse (0.57 records the attribute); disk.vhdx was not:
	// Hyper-V refuses sparse virtual disks, so it must not become one.
	root, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{
		{Name: "disk.img", Type: repo.NodeFile, Mode: 0o644, Size: size, Content: content, WinAttrs: fsutil.AttrSparse},
		{Name: "disk.vhdx", Type: repo.NodeFile, Mode: 0o644, Size: size, Content: content},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if _, err := restorer.Run(ctx, r, &repo.Snapshot{Tree: root}, restorer.Options{Target: target, Verify: true}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(target, "disk.img")
	fi, err := os.Stat(p)
	if err != nil || fi.Size() != int64(size) {
		t.Fatalf("size %v %v, want %d", fi, err, size)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for i := range content {
		if i == 4 {
			want = append(want, data...)
		} else {
			want = append(want, zeros...)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("content differs")
	}
	if used := allocated(t, p); used >= int64(size)/2 {
		t.Errorf("%d of %d bytes allocated: the zeros were written", used, size)
	}
	v := filepath.Join(target, "disk.vhdx")
	if b, err := os.ReadFile(v); err != nil || !bytes.Equal(b, want) {
		t.Fatalf("disk.vhdx: %v", err)
	}
	if fi, err := os.Stat(v); err != nil || fsutil.WinAttrs(fi)&fsutil.AttrSparse != 0 {
		t.Errorf("a file that was not sparse came back sparse: %v", err)
	}
}
