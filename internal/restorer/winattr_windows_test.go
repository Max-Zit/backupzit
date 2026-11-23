package restorer_test

import "syscall"

func winHidden(p string) bool {
	u, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return false
	}
	a, err := syscall.GetFileAttributes(u)
	return err == nil && a&syscall.FILE_ATTRIBUTE_HIDDEN != 0
}
