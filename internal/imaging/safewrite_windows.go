//go:build windows

package imaging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createStream opens the alternate data stream rel ("dir\file:stream")
// below root for writing (emptied); display names it. os.Root does not open
// stream names, so the file is opened (or created) through the root, and
// the stream is opened relative to that handle (NtCreateFile with the file
// as root directory): no path is resolved again, so no link swapped in
// meanwhile can lead elsewhere.
func createStream(root *os.Root, rel, display string) (*os.File, error) {
	base := streamBase(rel)
	bf, err := root.OpenFile(base, os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		bf, err = root.OpenFile(base, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	}
	if err != nil {
		return nil, err
	}
	defer bf.Close()                                         //nolint:errcheck // the handle the stream is opened relative to
	name, err := windows.NewNTUnicodeString(rel[len(base):]) // ":stream" or ":stream:$DATA"
	if err != nil {
		return nil, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(bf.Fd()),
		ObjectName:    name,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa)) // #nosec G103 -- size of the structure the API expects
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, windows.FILE_GENERIC_WRITE|windows.SYNCHRONIZE, oa, &iosb, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OVERWRITE_IF,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("open stream %s: %w", display, err)
	}
	return os.NewFile(uintptr(h), display), nil
}
