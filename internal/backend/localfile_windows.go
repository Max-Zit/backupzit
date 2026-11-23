package backend

import (
	"io/fs"
	"os"
	"syscall"
)

// objectFile reports whether fi (from Lstat or an open file) is a file that
// can hold an object of the repository: a regular file, also a deduplicated
// one, or another file-like reparse point such as a cloud file placeholder
// (Go reports those as irregular). Junctions and mount points are irregular
// too but carry the folder attribute: they are not objects.
func objectFile(fi fs.FileInfo) bool {
	m := fi.Mode()
	if m.IsRegular() {
		return true
	}
	if m&fs.ModeIrregular == 0 || m&(fs.ModeDir|fs.ModeSymlink|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeDevice|fs.ModeCharDevice) != 0 {
		return false
	}
	if a, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok && a.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return false
	}
	return true
}

// openObject opens the file p for reading.
func openObject(p string) (*os.File, error) {
	return os.Open(p) // #nosec G304 -- an object of the local repository, checked by the caller
}
