package hyperv

import (
	"path"
	"strings"
)

// DiskExcluded reports whether the job's exclusion patterns match a VM
// disk file. A pattern without a folder (*-data.vhdx, data.vhdx) is matched
// against the file name, one with a folder (D:\VMs\SQL01\data.vhdx,
// D:\Data\*.vhdx) against the full path. Matching ignores case and treats
// \ and / alike; * and ? are wildcards within one folder level.
func DiskExcluded(patterns []string, file string) bool {
	f := strings.ToLower(strings.ReplaceAll(file, `\`, "/"))
	base := f[strings.LastIndex(f, "/")+1:]
	for _, p := range patterns {
		p = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"))
		if p == "" {
			continue
		}
		target := base
		if strings.Contains(p, "/") {
			target = f
		}
		if ok, err := path.Match(p, target); ok || (err != nil && p == target) {
			return true
		}
	}
	return false
}
