package archiver

import (
	"io/fs"

	"github.com/max-zit/backupzit/internal/repo"
)

func unixMeta(string, fs.FileInfo) *repo.UnixMeta { return nil }

func deviceOf(fs.FileInfo) (uint64, bool) { return 0, false }
