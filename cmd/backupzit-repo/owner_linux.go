package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// fixOwner gives files created by root (add-key, fingerprint, undelete) to
// the owner and group of their directory. The storage directory is the
// service user's. The configuration directory is root:backupzit-repo since
// 0.56 (so the service cannot plant links there that root would follow):
// its files stay root's and are made readable for the group, which is how
// the service reads its keys and certificate. Links are never followed.
func fixOwner(paths ...string) {
	if os.Geteuid() != 0 {
		return
	}
	for _, p := range paths {
		fi, err := os.Stat(filepath.Dir(p))
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if li, err := os.Lstat(p); err != nil || !li.Mode().IsRegular() {
			continue
		}
		_ = os.Lchown(p, int(st.Uid), int(st.Gid))
		if st.Uid == 0 {
			_ = os.Chmod(p, 0o640) // #nosec G302 -- the service reads its keys and certificate through its group
		}
	}
}
