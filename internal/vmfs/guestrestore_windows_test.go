package vmfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
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

// Hyper-V refuses a sparse virtual disk (B9): a file with only short runs
// of zeros is not marked sparse on Windows.
func TestCopySparseShortRunsNotMarked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.vhdx")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	content := append(make([]byte, 4*sparseBlock), bytes.Repeat([]byte("data"), 1024)...)
	if _, err := copySparse(f, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		t.Fatal(err)
	}
	if attrs&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
		t.Error("a file with 4 MiB of zeros was marked sparse")
	}
	if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, content) {
		t.Errorf("content differs (%d bytes, want %d) %v", len(got), len(content), err)
	}
}
