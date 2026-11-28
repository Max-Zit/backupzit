//go:build !windows

package update

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// checkDirOwner: dir must belong to root or to the owner of the folder
// above it (the console's user), and others may not write to it (a request
// asks root to install, reboot or change the network).
func checkDirOwner(dir string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: owner unknown", dir)
	}
	if fi.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%s may be written by every user; the update helper does not use it", dir)
	}
	if st.Uid == 0 {
		return nil
	}
	parent, err := os.Stat(filepath.Dir(dir))
	if err != nil {
		return err
	}
	if pst, ok := parent.Sys().(*syscall.Stat_t); ok && pst.Uid == st.Uid {
		return nil
	}
	return fmt.Errorf("%s belongs to user %d, neither root nor the owner of %s; the update helper does not use it", dir, st.Uid, filepath.Dir(dir))
}
