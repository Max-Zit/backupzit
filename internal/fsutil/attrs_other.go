//go:build !windows

package fsutil

import "io/fs"

func WinAttrs(fs.FileInfo) uint32      { return 0 }
func SetWinAttrs(string, uint32) error { return nil }
func ClearReadOnly(string)             {}
