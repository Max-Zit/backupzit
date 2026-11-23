//go:build !linux

package archiver

// pseudoMounts: only Linux mounts kernel file systems below / (L-32).
func pseudoMounts() map[string]bool { return nil }
