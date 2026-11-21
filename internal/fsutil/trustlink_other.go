//go:build !windows

package fsutil

import (
	"io/fs"
	"os"
	"syscall"
)

// TrustedLink reports whether the symbolic link at path belongs to root or
// to the agent's own user, like the links of the system itself. Restores
// to the original location follow only such links.
func TrustedLink(_ string, fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Uid == 0 || int(st.Uid) == os.Geteuid()
}
