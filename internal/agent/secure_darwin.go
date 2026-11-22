package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

// protectDir makes dir accessible to its owner (root) only. On macOS the
// mode is not the whole answer: a folder can also carry ACL entries, and
// a folder created below one with inheritable entries (set by an MDM
// profile or an administrator on /Library/Application Support) gets them
// too, granting access that 0700 does not show. They are removed, so the
// configuration (the agent's secret) and the update folder stay root's.
func protectDir(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a directory needs the x bit
		return err
	}
	return clearACL(dir)
}

// clearACL removes every ACL entry of dir. Go has no access to the ACL
// API, and /bin/chmod -N is the system's own way (it succeeds on a folder
// without entries).
func clearACL(dir string) error {
	out, err := exec.Command("/bin/chmod", "-N", dir).CombinedOutput() // #nosec G204 -- fixed program; dir is one of the agent's own folders
	if err != nil {
		return fmt.Errorf("remove the access control entries of %s: %w: %s", dir, err, bytes.TrimSpace(out))
	}
	return nil
}
