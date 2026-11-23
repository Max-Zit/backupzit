//go:build !windows

package backend

import (
	"io/fs"
	"os"
	"syscall"
)

// objectFile reports whether fi (from Lstat or an open file) is a file that
// can hold an object of the repository.
func objectFile(fi fs.FileInfo) bool { return fi.Mode().IsRegular() }

// openObject opens the regular file p for reading without following a link
// put there meanwhile; a named pipe opens without waiting for a writer, so
// the type check after the open refuses it instead of hanging.
func openObject(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- an object of the local repository, checked by the caller
}
