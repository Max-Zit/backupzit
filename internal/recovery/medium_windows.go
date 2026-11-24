package recovery

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// BootMediumPaths returns the roots of the medium Windows PE was started
// from, which must never become a restore target: the drive Windows PE
// names in PEBootRamdiskSourceDrive, and every drive that holds a recovery
// medium's files (\sources\boot.wim, \backupzit\recovery.json). The
// recovery program itself runs from the RAM disk X: and belongs to no disk,
// so its own path protects nothing.
func BootMediumPaths() []string {
	var out []string
	seen := map[string]bool{}
	add := func(root string) {
		root = strings.ToUpper(strings.TrimRight(root, `\`)) + `\`
		if len(root) == 3 && root[1] == ':' && !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetStringValue("PEBootRamdiskSourceDrive"); err == nil {
			add(v)
		}
		_ = k.Close()
	}
	for _, root := range DriveRoots() {
		for _, f := range []string{`sources\boot.wim`, `backupzit\recovery.json`} {
			if _, err := os.Stat(filepath.Join(root, f)); err == nil {
				add(root)
			}
		}
	}
	return out
}
