package restorer

import (
	"errors"
	"io/fs"

	"github.com/max-zit/backupzit/internal/repo"
)

func applyUnix(string, *repo.Node, bool) []error { return nil }

func makeSpecial(string, *repo.Node) error {
	return errors.New("Unix device files and named pipes cannot be restored on Windows")
}

// trustedLink: creating symbolic links on Windows needs administrator
// rights (or developer mode), so links at the original location are the
// system's or an administrator's.
func trustedLink(fs.FileInfo) bool { return true }
