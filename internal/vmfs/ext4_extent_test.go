package vmfs

import (
	"testing"

	"github.com/masahiro331/go-ext4-filesystem/ext4"
)

// TestExt4FullLengthExtent guards the ext4 reader against taking an extent
// of the maximum length (32768 blocks, 128 MiB with 4 KiB blocks) for an
// unwritten one. The kernel writes such extents for most large files; the
// reader then restored them as zeros while the restore reported success.
// Only lengths above 32768 mark unwritten extents (ext4_ext_is_unwritten).
func TestExt4FullLengthExtent(t *testing.T) {
	for _, tc := range []struct {
		len       uint16
		unwritten bool
		blocks    uint16
	}{
		{1, false, 1},
		{0x7FFF, false, 0x7FFF},
		{0x8000, false, 0x8000},
		{0x8001, true, 1},
		{0xFFFF, true, 0x7FFF},
	} {
		e := ext4.Extent{Len: tc.len}
		if got := e.IsUninitialized(); got != tc.unwritten {
			t.Errorf("Len %#x: unwritten = %v, want %v", tc.len, got, tc.unwritten)
		}
		if got := e.GetLen(); got != tc.blocks {
			t.Errorf("Len %#x: %d blocks, want %d", tc.len, got, tc.blocks)
		}
	}
}
