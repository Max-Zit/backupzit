//go:build !windows

package agent

import "os"

// protectDir makes dir accessible to its owner (root) only.
func protectDir(dir string) error { return os.Chmod(dir, 0o700) } // #nosec G302 -- a directory needs the x bit
