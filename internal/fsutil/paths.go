// Package fsutil maps between OS paths and the slash-separated path layout
// used inside snapshots.
//
// Inside a snapshot every source is stored under its full absolute path:
//
//	C:\Users\a\Docs      -> C/Users/a/Docs
//	\\srv\share\x        -> UNC/srv/share/x
//	/home/a              -> home/a
package fsutil

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// SnapshotComponents converts an OS path into snapshot path components.
func SnapshotComponents(p string) ([]string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	vol := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, vol)
	var comps []string
	switch {
	case vol == "":
	case len(vol) == 2 && vol[1] == ':':
		comps = append(comps, strings.ToUpper(vol[:1]))
	case strings.HasPrefix(vol, `\\`):
		parts := strings.Split(strings.Trim(vol, `\`), `\`)
		comps = append(comps, "UNC")
		comps = append(comps, parts...)
	default:
		return nil, errors.New("unsupported volume " + vol)
	}
	for _, c := range strings.Split(filepath.ToSlash(rest), "/") {
		if c != "" {
			comps = append(comps, c)
		}
	}
	return comps, nil
}

// ParseSnapshotPath accepts either an OS path ("C:\Users") or a snapshot path
// ("C/Users", "/C/Users") and returns its components.
func ParseSnapshotPath(p string) ([]string, error) {
	if runtime.GOOS == "windows" && (filepath.VolumeName(p) != "") {
		return SnapshotComponents(p)
	}
	var comps []string
	for _, c := range strings.Split(strings.ReplaceAll(p, `\`, "/"), "/") {
		if c != "" && c != "." {
			comps = append(comps, c)
		}
	}
	return comps, nil
}

// OriginalPath converts snapshot components back to the OS path they were
// backed up from.
func OriginalPath(comps []string) string {
	if len(comps) == 0 {
		if runtime.GOOS == "windows" {
			return ""
		}
		return "/"
	}
	if runtime.GOOS == "windows" {
		first := comps[0]
		if len(first) == 1 {
			return filepath.Join(append([]string{first + `:\`}, comps[1:]...)...)
		}
		if first == "UNC" && len(comps) >= 3 {
			return `\\` + filepath.Join(comps[1:]...)
		}
	}
	return "/" + filepath.Join(comps...)
}
