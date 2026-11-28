//go:build !windows && !darwin

package fsutil

import "os"

// MarkSparse: Unix file systems leave ranges skipped with a seek
// unallocated in any file.
func MarkSparse(*os.File) error { return nil }

// PunchHoles: the ranges skipped with a seek stay unallocated.
func PunchHoles(*os.File, []Hole) error { return nil }
