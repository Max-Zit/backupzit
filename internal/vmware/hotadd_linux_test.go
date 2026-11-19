package vmware

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeDisk(t *testing.T, root, name string, size uint64) {
	t.Helper()
	dir := filepath.Join(root, "block", name, "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "block", name, "size"), []byte(strconv.FormatUint(size/512, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The kernel's control file exists already (a test as non-root could
	// not read one created write-only).
	if err := os.WriteFile(filepath.Join(dir, "delete"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The attached disk is the one new disk of its size; several, or none in
// time, is an error rather than a guess.
func TestWaitForDisk(t *testing.T) {
	root := t.TempDir()
	old, oldWait := sysfs, diskWait
	sysfs, diskWait = root, 3*time.Second
	t.Cleanup(func() { sysfs, diskWait = old, oldWait })
	fakeDisk(t, root, "sda", 32<<30)
	fakeDisk(t, root, "loop0", 8<<30) // not a disk
	before, err := blockDevices()
	if err != nil || len(before) != 1 {
		t.Fatalf("before: %v %v", before, err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		fakeDisk(t, root, "sdc", 4<<30) // another size: not it
		fakeDisk(t, root, "sdb", 8<<30)
	}()
	dev, err := waitForDisk(context.Background(), before, 8<<30)
	if err != nil || dev != "/dev/sdb" {
		t.Fatalf("dev = %q, %v", dev, err)
	}
	fakeDisk(t, root, "sdd", 8<<30)
	if _, err := waitForDisk(context.Background(), before, 8<<30); err == nil || !strings.Contains(err.Error(), "several new disks") {
		t.Fatalf("two candidates: %v", err)
	}
	diskWait = 500 * time.Millisecond
	if _, err := waitForDisk(context.Background(), before, 16<<30); err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Fatalf("none: %v", err)
	}
	// Releasing removes the device from the kernel.
	releaseDisk("/dev/sdb", func(string, ...any) {})
	if b, _ := os.ReadFile(filepath.Join(root, "block", "sdb", "device", "delete")); string(b) != "1" {
		t.Errorf("device not deleted: %q", b)
	}
}

func TestOwnVMUUIDs(t *testing.T) {
	root := t.TempDir()
	old := sysfs
	sysfs = root
	t.Cleanup(func() { sysfs = old })
	if err := os.MkdirAll(filepath.Join(root, "class/dmi/id"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "class/dmi/id/product_uuid"), []byte("564D1234-5678-9ABC-DEF0-123456789ABC\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	ids, err := ownVMUUIDs()
	if err != nil || len(ids) != 2 || ids[0] != "564d1234-5678-9abc-def0-123456789abc" || ids[1] != "34124d56-7856-bc9a-def0-123456789abc" {
		t.Fatalf("ids = %v, %v", ids, err)
	}
}
