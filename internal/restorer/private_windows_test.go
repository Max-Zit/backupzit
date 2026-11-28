package restorer_test

import "io/fs"

// ownerOf: Windows files have no Unix owner.
func ownerOf(fs.FileInfo) (int, bool) { return 0, false }

// hasXattr: no extended attributes on Windows.
func hasXattr(string, string) bool { return false }
