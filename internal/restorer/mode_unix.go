//go:build !windows

package restorer

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// unixMode converts Go file mode flags to the Unix st_mode permission bits
// including setuid, setgid and sticky.
func unixMode(m fs.FileMode) uint32 {
	mode := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= unix.S_ISUID
	}
	if m&fs.ModeSetgid != 0 {
		mode |= unix.S_ISGID
	}
	if m&fs.ModeSticky != 0 {
		mode |= unix.S_ISVTX
	}
	return mode
}
