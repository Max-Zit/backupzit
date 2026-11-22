//go:build !windows

package agent

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// privateDir makes sure the update folder is root's own: a real folder
// (not a link), owned by the agent's user, writable by nobody else, so
// that nobody can swap the checked package before it is installed.
func privateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not a folder owned by the agent's user; refusing to install from it", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700) // #nosec G302 -- a folder: 0700 is root only
	}
	return nil
}
