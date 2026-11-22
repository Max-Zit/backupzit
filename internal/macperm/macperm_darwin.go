package macperm

import (
	"errors"
	"io/fs"
	"os"
)

// protectedProbes are files and folders that exist on every Mac and that
// only a process with Full Disk Access may open: the system's privacy
// database first, then folders of the system user profile.
var protectedProbes = []string{
	"/Library/Application Support/com.apple.TCC/TCC.db",
	"/Library/Preferences/com.apple.TimeMachine.plist",
}

func fullDiskAccess() State {
	denied := false
	for _, p := range protectedProbes {
		f, err := os.Open(p) // #nosec G304 -- fixed system paths, opened read-only to test the permission
		if err == nil {
			_ = f.Close()
			return Granted
		}
		switch {
		case errors.Is(err, fs.ErrPermission):
			denied = true
		case errors.Is(err, fs.ErrNotExist):
			// Not on this Mac: try the next one.
		}
	}
	if denied {
		return Denied
	}
	return Unknown
}
