package restorer

import (
	"os"

	"golang.org/x/sys/windows"
)

// markSparse marks f as a sparse file. NTFS leaves the ranges skipped with
// a seek unallocated only in a sparse file; in any other file it fills them
// with zeros on disk, and a restored VM image or database takes its full
// size (B9). On a file system without sparse files (FAT, exFAT) it fails,
// and the skipped ranges are filled with zeros as before.
func markSparse(f *os.File) error {
	var ret uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &ret, nil)
}
