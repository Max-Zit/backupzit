//go:build !windows && !darwin

package restorer

import "os"

// markSparse: Unix file systems leave ranges skipped with a seek
// unallocated in any file.
func markSparse(*os.File) error { return nil }

// punchHoles: the ranges skipped with a seek stay unallocated.
func punchHoles(*os.File, []hole) error { return nil }
