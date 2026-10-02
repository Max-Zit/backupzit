package restorer

import (
	"errors"

	"github.com/backupzit/backupzit/internal/repo"
)

func applyUnix(string, *repo.Node, bool) []error { return nil }

func makeSpecial(string, *repo.Node) error {
	return errors.New("Unix device files and named pipes cannot be restored on Windows")
}
