package pve

import (
	"os"
	"path/filepath"
	"syscall"
)

// mountedDir reports whether dir is a mounted file system of its own (a
// ZFS subvolume), not a plain folder of the one above it: an unmounted
// dataset leaves its mount point as an empty folder of the parent, and
// files restored there would land on the parent file system.
var mountedDir = func(dir string) bool {
	a, err := os.Stat(dir)
	if err != nil || !a.IsDir() {
		return false
	}
	b, err := os.Stat(filepath.Dir(dir))
	if err != nil {
		return false
	}
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Dev != sb.Dev
}
