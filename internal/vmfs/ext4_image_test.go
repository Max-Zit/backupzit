package vmfs

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"
)

// TestExt4ImageLargeFile reads a large file from a real ext4 image made by
// mkfs.ext4 (lab: BACKUPZIT_TEST_EXT4_IMAGE, BACKUPZIT_TEST_EXT4_FILE,
// BACKUPZIT_TEST_EXT4_SHA256). Files above 128 MiB have extents of the full
// length, which were read as zeros (#49).
func TestExt4ImageLargeFile(t *testing.T) {
	img, name, want := os.Getenv("BACKUPZIT_TEST_EXT4_IMAGE"), os.Getenv("BACKUPZIT_TEST_EXT4_FILE"), os.Getenv("BACKUPZIT_TEST_EXT4_SHA256")
	if img == "" {
		t.Skip("BACKUPZIT_TEST_EXT4_IMAGE not set")
	}
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	vols, err := Volumes(f, st.Size())
	if err != nil || len(vols) == 0 {
		t.Fatalf("volumes: %v %v", vols, err)
	}
	fsys, err := Open(vols[0])
	if err != nil {
		t.Fatal(err)
	}
	r, size, err := fsys.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil || n != size {
		t.Fatalf("read %d of %d bytes: %v", n, size, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		t.Fatalf("%s: sha256 %s, want %s", name, got, want)
	}
}
