//go:build !darwin

package restorer

import "github.com/max-zit/backupzit/internal/fsutil"

// unlockDir: only macOS restores flags that lock a folder (flags_darwin.go).
func unlockDir(*fsutil.Dir) (relock func()) { return func() {} }
