//go:build darwin

package archiver

import "os"

// systemSkips returns the folders kept empty in this macOS backup of
// roots: the fixed rules and the data volume's folders that roots already
// read through their firmlinks. Without /usr/share/firmlinks (older macOS
// had no data volume) only the fixed rules apply.
func systemSkips(roots []string) *skipSet {
	var dups map[string]bool
	if b, err := os.ReadFile("/usr/share/firmlinks"); err == nil {
		dups = firmlinkDuplicates(string(b), roots)
	}
	return newSkipSet(skipPatterns(macOSSkips), dups)
}
