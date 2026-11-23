//go:build !windows && !darwin

package agent

import "os"

// protectDir makes dir accessible to its owner (root) only.
func protectDir(dir string) error { return os.Chmod(dir, 0o700) } // #nosec G302 -- a directory needs the x bit

// clearACL: Linux folders of the agent get no access control entries from
// their parent (only macOS's do, see secure_darwin.go).
func clearACL(string) error { return nil }
