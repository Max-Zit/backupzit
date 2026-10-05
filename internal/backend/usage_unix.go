//go:build !windows

package backend

import "golang.org/x/sys/unix"

func diskUsage(dir string) (Usage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return Usage{}, err
	}
	return Usage{Total: uint64(st.Blocks) * uint64(st.Bsize), Free: uint64(st.Bavail) * uint64(st.Bsize)}, nil
}
