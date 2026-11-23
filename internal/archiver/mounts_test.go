package archiver

import "testing"

// P2 (0.55 review): a path the job lists in ExtraMounts is read even when
// it is another file system or is seen as a kernel file system.
func TestStoreEmptyExtraMounts(t *testing.T) {
	root := uint64(1)
	a := &Archiver{
		opts:      Options{OneFileSystem: true, ExtraMounts: []string{"/boot", "/boot/efi"}},
		parentDev: &root,
		pseudo:    map[string]bool{"/boot": true, "/proc": true},
	}
	for _, p := range []string{"/boot", "/boot/efi"} {
		if a.storeEmpty(p, 2) {
			t.Errorf("%s is listed in ExtraMounts but stored empty", p)
		}
	}
	if !a.storeEmpty("/mnt/usb", 3) {
		t.Error("another file system not listed in ExtraMounts is read with OneFileSystem")
	}
	if !a.storeEmpty("/proc", 1) {
		t.Error("a kernel file system is read")
	}
	if a.storeEmpty("/etc", 1) {
		t.Error("a folder on the same file system is stored empty")
	}
}
