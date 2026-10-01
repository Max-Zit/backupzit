//go:build windows

package fsutil

import (
	"io/fs"
	"syscall"
)

// Windows attributes we preserve. Others (archive, compressed, encrypted,
// reparse point, ...) are either transient or need special handling.
const preservedAttrs = syscall.FILE_ATTRIBUTE_READONLY |
	syscall.FILE_ATTRIBUTE_HIDDEN |
	syscall.FILE_ATTRIBUTE_SYSTEM

// WinAttrs returns the preserved Windows file attributes of fi.
func WinAttrs(fi fs.FileInfo) uint32 {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes & preservedAttrs
	}
	return 0
}

// SetWinAttrs applies preserved attributes to path.
func SetWinAttrs(path string, attrs uint32) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	cur, err := syscall.GetFileAttributes(p)
	if err != nil {
		return err
	}
	next := (cur &^ preservedAttrs) | (attrs & preservedAttrs)
	if next == cur {
		return nil
	}
	if next == 0 {
		next = syscall.FILE_ATTRIBUTE_NORMAL
	}
	return syscall.SetFileAttributes(p, next)
}

// ClearReadOnly removes the read-only attribute so a file can be overwritten.
func ClearReadOnly(path string) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	cur, err := syscall.GetFileAttributes(p)
	if err != nil || cur&syscall.FILE_ATTRIBUTE_READONLY == 0 {
		return
	}
	_ = syscall.SetFileAttributes(p, cur&^syscall.FILE_ATTRIBUTE_READONLY)
}
