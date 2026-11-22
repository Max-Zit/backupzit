//go:build darwin

package archiver

import (
	"syscall"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

// statExtra records the BSD file flags (hidden, nodump, immutable, ...) and
// the creation time of a macOS entry. The flags are stored as read, also
// UF_COMPRESSED: which of them can be set again is decided at restore time
// (the restore never sets UF_COMPRESSED, the archiver reads and stores the
// decompressed content; the kernel hides the com.apple.decmpfs attribute
// and a resource fork used for compression from listxattr, so they are
// never stored).
func statExtra(m *repo.UnixMeta, st *syscall.Stat_t) {
	m.Flags = st.Flags
	// File systems without a creation time (some network shares) report
	// zero: the node then has none rather than 1970.
	if bt := st.Birthtimespec; bt.Sec > 0 || bt.Nsec > 0 {
		m.Birthtime = time.Unix(bt.Sec, bt.Nsec).UTC()
	}
}
