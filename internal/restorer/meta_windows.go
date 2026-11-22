//go:build windows

package restorer

import (
	"errors"
	"os"
	"time"

	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
)

// Windows attributes and times are set on the entry opened relative to its
// folder, as it is (a link put at the name is changed itself, nothing it
// points to), never by path.

// applyFileMeta: the attributes come after the rename (afterRename).
func applyFileMeta(*os.File, *repo.Node, bool) []error { return nil }

// afterRename sets attributes and times of the restored file name in d.
// Attributes are always applied: zero means "no preserved attributes set"
// and must clear e.g. the archive bit Windows put on the new file.
func afterRename(d *fsutil.Dir, name string, _ fsutil.FileID, n *repo.Node) []error {
	if err := d.SetMeta(name, n.WinAttrs, true, n.ModTime); err != nil {
		return []error{err}
	}
	return nil
}

// applyDirMeta sets attributes and times of the restored folder name in
// parent.
func applyDirMeta(parent, _ *fsutil.Dir, name string, n *repo.Node, setTimes bool) []error {
	if parent == nil {
		return nil
	}
	var mt time.Time
	if setTimes {
		mt = n.ModTime
	}
	if err := parent.SetMeta(name, n.WinAttrs, true, mt); err != nil {
		return []error{err}
	}
	return nil
}

// applySelfMeta sets attributes and times of RunDir's target folder,
// chosen by the agent (by its path, as before).
func applySelfMeta(d *fsutil.Dir, n *repo.Node) []error {
	var errs []error
	if !n.ModTime.IsZero() {
		if err := os.Chtimes(d.Path(), n.ModTime, n.ModTime); err != nil {
			errs = append(errs, err)
		}
	}
	if err := fsutil.SetWinAttrs(d.Path(), n.WinAttrs); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// applyLinkMeta: symbolic links keep the attributes they get.
func applyLinkMeta(*fsutil.Dir, string, *repo.Node) []error { return nil }

func makeSpecialAt(*fsutil.Dir, string, *repo.Node) []error {
	return []error{errors.New("Unix device files and named pipes cannot be restored on Windows")}
}
