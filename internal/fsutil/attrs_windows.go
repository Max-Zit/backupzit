//go:build windows

package fsutil

import (
	"io/fs"
	"syscall"
)

const fileAttributeNotContentIndexed = 0x2000

// Windows attributes we preserve. Others (compressed, encrypted, sparse,
// reparse point, ...) need special handling and are not restored yet.
// Archive is restored too: Windows sets it on every new file, so without
// it restored files would differ from the originals.
const preservedAttrs = syscall.FILE_ATTRIBUTE_READONLY |
	syscall.FILE_ATTRIBUTE_HIDDEN |
	syscall.FILE_ATTRIBUTE_SYSTEM |
	syscall.FILE_ATTRIBUTE_ARCHIVE |
	fileAttributeNotContentIndexed

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
