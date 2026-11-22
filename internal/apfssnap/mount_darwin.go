package apfssnap

import "golang.org/x/sys/unix"

const available = true

// mountOf returns the mount point of the file system holding p (statfs
// follows firmlinks: /Users is on /System/Volumes/Data).
func mountOf(p string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(st.Mntonname[:]), nil
}
