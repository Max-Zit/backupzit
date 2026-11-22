//go:build !darwin

package apfssnap

import "errors"

// APFS local snapshots exist on macOS only; elsewhere every path is read
// as before (VSS on Windows).
const available = false

func mountOf(string) (string, error) {
	return "", errors.New("APFS local snapshots are only available on macOS")
}
