//go:build !darwin

package apfssnap

import (
	"errors"
	"os"
)

// APFS local snapshots exist on macOS only; elsewhere every path is read
// as before (VSS on Windows).
const available = false

func mountOf(string) (string, error) {
	return "", errors.New("APFS local snapshots are only available on macOS")
}

// ownedByUs: no snapshot folders outside macOS.
func ownedByUs(os.FileInfo) bool { return false }
