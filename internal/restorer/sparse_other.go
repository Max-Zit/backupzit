//go:build !windows

package restorer

import "os"

// markSparse: Unix file systems leave ranges skipped with a seek
// unallocated in any file.
func markSparse(*os.File) error { return nil }
