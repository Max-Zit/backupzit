package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// fixOwner gives files created by root (add-key, fingerprint, undelete) to
// the owner of their directory, the service user.
func fixOwner(paths ...string) {
	if os.Geteuid() != 0 {
		return
	}
	for _, p := range paths {
		fi, err := os.Stat(filepath.Dir(p))
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			os.Chown(p, int(st.Uid), int(st.Gid))
		}
	}
}
