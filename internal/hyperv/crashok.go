package hyperv

import (
	"path"
	"strings"
)

// CrashConsistentExpected reports whether a VM is listed among the VMs whose
// crash-consistent checkpoint is expected (guests that cannot prepare one,
// such as old Linux without the Hyper-V daemons; issue #41), by its name or
// a pattern such as "legacy-*", case-insensitive like Hyper-V names.
func CrashConsistentExpected(name string, patterns []string) bool {
	n := strings.ToLower(name)
	for _, p := range patterns {
		if ok, err := path.Match(strings.ToLower(strings.TrimSpace(p)), n); err == nil && ok {
			return true
		}
	}
	return false
}
