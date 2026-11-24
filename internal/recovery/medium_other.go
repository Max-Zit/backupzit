//go:build !windows

package recovery

// BootMediumPaths returns nothing outside Windows: only the Windows
// recovery medium is protected this way.
func BootMediumPaths() []string { return nil }
