//go:build !darwin && !windows

package archiver

import (
	"io/fs"

	"github.com/max-zit/backupzit/internal/repo"
)

// Linux has no file provider that keeps files dataless.
func entryInCloud(fs.FileInfo, *repo.Node) bool { return false }

func pathInCloud(string) bool { return false }
