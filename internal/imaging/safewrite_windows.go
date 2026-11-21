//go:build windows

package imaging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// createStream opens the alternate data stream rel ("dir\file:stream")
// below the restore folder. os.Root does not open stream names, so the
// file is opened (or created) through the root first, then the stream by
// its full path, and both handles must name the same file: a link swapped
// in between would make the stream open another file, which is closed
// again before anything is written.
func (t *Target) createStream(rel string) (*os.File, error) {
	base := streamBase(rel)
	bf, err := t.root.OpenFile(base, os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		bf, err = t.root.OpenFile(base, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	}
	if err != nil {
		return nil, err
	}
	defer bf.Close()                                                                  //nolint:errcheck // read handle, only compared
	sf, err := os.OpenFile(filepath.Join(t.dir, rel), os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- checked against the file opened through the root below
	if err != nil {
		return nil, err
	}
	same, err := sameFile(bf, sf)
	if err != nil || !same {
		_ = sf.Close()
		if err == nil {
			err = errors.New("the file changed while its stream was restored")
		}
		return nil, fmt.Errorf("%s: %w", filepath.Join(t.dir, rel), err)
	}
	if err := sf.Truncate(0); err != nil {
		_ = sf.Close()
		return nil, err
	}
	return sf, nil
}

// sameFile tells whether two handles are of one file (a stream handle has
// the file's volume and index).
func sameFile(a, b *os.File) (bool, error) {
	var ia, ib windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(a.Fd()), &ia); err != nil {
		return false, err
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(b.Fd()), &ib); err != nil {
		return false, err
	}
	return ia.VolumeSerialNumber == ib.VolumeSerialNumber && ia.FileIndexHigh == ib.FileIndexHigh && ia.FileIndexLow == ib.FileIndexLow, nil
}
