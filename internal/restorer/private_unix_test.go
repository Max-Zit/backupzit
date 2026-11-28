//go:build !windows

package restorer_test

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

// ownerOf is the user who owns fi.
func ownerOf(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// hasXattr reports whether p has the extended attribute name.
func hasXattr(p, name string) bool {
	_, err := unix.Getxattr(p, name, nil)
	return err == nil
}
