//go:build windows

package archiver

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/max-zit/backupzit/internal/repo"
)

// Cloud files placeholders (OneDrive Files On-Demand and other sync apps
// on the Cloud Files API): an "online-only" file has its content in the
// cloud only. Windows hides the placeholder's reparse point, so it looks
// like a plain file; these attributes tell. A file kept "on this device"
// has neither.
const (
	attrRecallOnOpen       = 0x00040000 // FILE_ATTRIBUTE_RECALL_ON_OPEN
	attrRecallOnDataAccess = 0x00400000 // FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS
	cloudAttrs             = attrRecallOnOpen | attrRecallOnDataAccess
)

func entryInCloud(fi fs.FileInfo, _ *repo.Node) bool {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && d.FileAttributes&cloudAttrs != 0
}

func pathInCloud(p string) bool {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return false
	}
	a, err := windows.GetFileAttributes(u)
	return err == nil && a != windows.INVALID_FILE_ATTRIBUTES && a&cloudAttrs != 0
}
