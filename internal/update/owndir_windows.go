package update

import "os"

// checkDirOwner: the root helper runs on Linux only (systemd); on Windows
// the folder's link check and os.Root are what applies.
func checkDirOwner(string, os.FileInfo) error { return nil }
