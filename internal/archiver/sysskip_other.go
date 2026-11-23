//go:build !darwin

package archiver

// systemSkips: Linux and Windows backups read every folder they are given
// (Linux kernel file systems are handled by pseudoMounts).
func systemSkips([]string) *skipSet { return nil }
