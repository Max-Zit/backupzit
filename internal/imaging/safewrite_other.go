//go:build !windows

package imaging

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/max-zit/backupzit/internal/fsutil"
)

// createStream: alternate data streams exist only on Windows (Create never
// asks for one elsewhere).
func createStream(*os.Root, string, string) (*os.File, error) {
	return nil, errors.New("alternate data streams are written only on Windows")
}

// chmodDir sets the permissions of the folder rel in root through a handle
// of the folder itself (a folder cannot be a hard link).
func chmodDir(root *os.Root, rel string, m fs.FileMode) error {
	f, err := openDir(root, rel)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // metadata only
	return f.Chmod(m)
}

// chtimesDir sets the times of the folder rel in root through its handle.
func chtimesDir(root *os.Root, rel string, mtime time.Time) error {
	f, err := openDir(root, rel)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // metadata only
	return fsutil.SetTimes(f, mtime)
}

func openDir(root *os.Root, rel string) (*os.File, error) {
	r, err := root.OpenRoot(rel) // a folder, not followed out of root
	if err != nil {
		return nil, err
	}
	defer r.Close() //nolint:errcheck // the handle below stays open
	return r.Open(".")
}
