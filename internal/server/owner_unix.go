//go:build !windows

package server

import (
	"os"
	"syscall"
)

// matchOwner gives path the owner of dir (files restored as root for the service user).
func matchOwner(path, dir string) {
	fi, err := os.Stat(dir)
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		os.Chown(path, int(st.Uid), int(st.Gid))
	}
}
