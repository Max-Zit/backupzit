package fsutil

import (
	"os"

	"golang.org/x/sys/unix"
)

// MarkSparse: APFS needs no mark (see PunchHoles).
func MarkSparse(*os.File) error { return nil }

// PunchHoles frees the ranges of zeros skipped while writing f. APFS fills
// a gap below 16 MiB with zeros on disk when data is written after it, and
// the gap a truncate adds at the end, so a restored VM image or database
// would take its full size. Only whole blocks inside each range are freed.
// fpunchhole_t has the layout of the first fields of fstore_t (flags,
// reserved, offset, length), so FcntlFstore passes it.
func PunchHoles(f *os.File, holes []Hole) error {
	if len(holes) == 0 {
		return nil
	}
	const block = 4096
	for _, h := range holes {
		start := (h.Off + block - 1) / block * block
		end := (h.Off + h.N) / block * block
		if end <= start {
			continue
		}
		if err := unix.FcntlFstore(f.Fd(), unix.F_PUNCHHOLE, &unix.Fstore_t{Offset: start, Length: end - start}); err != nil {
			return err
		}
	}
	return nil
}
